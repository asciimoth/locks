package locks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func FuzzID(f *testing.F) {
	for _, s := range idSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, e := ParseID(s)
		if e != nil {
			return
		}
		if string(id) != s {
			t.Fatal("identity changed")
		}
		digest, alg, _ := strings.Cut(s, ":")
		b, e := b32.DecodeString(digest)
		if e != nil || b32.EncodeToString(b) != digest || !validName(alg) {
			t.Fatal("noncanonical acceptance")
		}
		again, e := ParseID(string(id))
		if e != nil || again != id {
			t.Fatal("unstable ID")
		}
	})
}
func FuzzValue(f *testing.F) {
	for _, s := range valueSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, e := NormalizeValue(s)
		if e != nil {
			return
		}
		again, e := NormalizeValue(v + "\r\r")
		if e != nil || again != v {
			t.Fatal("normalization not idempotent")
		}
	})
}
func FuzzKey(f *testing.F) {
	for _, s := range keySeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, l := range []Limits{{}, {RawKeyBytes: 2}, {RawKeyBytes: -1}} {
			id, e := ParseKey(s, l)
			if e != nil {
				continue
			}
			checked, e := ParseID(string(id))
			if e != nil || checked != id {
				t.Fatal("invalid resolved ID")
			}
			if strings.HasPrefix(s, "raw.") {
				_, algEnc, _ := strings.Cut(s, ".")
				alg, _, _ := strings.Cut(algEnc, ".")
				if alg != "blake2" && alg != "sha256" {
					t.Fatal("unsupported raw algorithm")
				}
			}
		}
	})
}
func FuzzLeaf(f *testing.F) {
	for _, s := range leafSeeds() {
		for _, p := range []string{"", "XX", "XY", "bad", strings.Repeat("A", 53)} {
			f.Add(s, p)
		}
	}
	f.Fuzz(func(t *testing.T, s, p string) {
		for _, limits := range []Limits{{}, {ScanBytes: -1}, {ScanBytes: 4}, {LineBytes: 3}, {ValueBytes: 1}} {
			leaf, e := DecodeLeaf(strings.NewReader(s), p, limits)
			if e != nil {
				continue
			}
			var b bytes.Buffer
			if e = EncodeLeaf(&b, leaf.Records); e != nil {
				t.Fatal(e)
			}
			again, e := DecodeLeaf(&b, p, Limits{})
			if e != nil || !reflect.DeepEqual(leaf.Records, again.Records) {
				t.Fatal("leaf round trip", e)
			}
		}
		if _, e := DecodeLeaf(failedIO{}, "", Limits{}); !errors.Is(e, io.ErrUnexpectedEOF) {
			t.Fatal(e)
		}
	})
}
func FuzzRequest(f *testing.F) {
	for _, s := range requestSeeds() {
		f.Add(s)
	}
	f.Add("locks/1 get\nraw.blake2.YQ\nraw.blake2.Yg\n")
	f.Add("locks/1 put\ncreate raw.blake2.YQ long\n")
	f.Fuzz(func(t *testing.T, s string) {
		for _, l := range []Limits{{}, {BodyBytes: -1}, {BodyBytes: 2}, {LineBytes: 3}, {Entries: 1}, {ValueBytes: 1}} {
			req, e := DecodeRequest(strings.NewReader(s), l)
			if e != nil {
				continue
			}
			again, e := DecodeRequest(strings.NewReader(encodeRequest(req)), Limits{})
			if e != nil || !reflect.DeepEqual(again, req) {
				t.Fatal("request round trip", e)
			}
		}
	})
}
func FuzzResults(f *testing.F) {
	for _, s := range resultSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, req := range []Request{{}, {Items: []Write{{ID: abc}}}, {Put: true, Items: []Write{{ID: abc, Mode: Create}}}, {Put: true, Items: []Write{{ID: abc, Mode: Replace}}}} {
			for _, l := range []Limits{{}, {BodyBytes: -1}, {ValueBytes: 1}} {
				out, e := DecodeResults(strings.NewReader(s), req, l)
				if e != nil {
					continue
				}
				encoded, e := encodeResults(out, Limits{})
				if e != nil {
					t.Fatal(e)
				}
				again, e := DecodeResults(strings.NewReader(encoded), req, Limits{})
				if e != nil || fmt.Sprint(again) != fmt.Sprint(out) {
					t.Fatal("results round trip", e)
				}
			}
		}
	})
}
func FuzzError(f *testing.F) {
	for _, s := range errorSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, status := range []int{400, 413} {
			for _, l := range []Limits{{}, {Entries: 32}, {Entries: 1}, {BodyBytes: -1}} {
				e := DecodeError(strings.NewReader(s), status, l)
				if e == nil {
					t.Fatal("nil rejection")
				}
				var many *TooManyError
				if errors.As(e, &many) {
					if status != 413 || many.Max < 1 {
						t.Fatal(e)
					}
				}
			}
		}
	})
}
func FuzzPath(f *testing.F) {
	for _, s := range pathSeeds() {
		f.Add(s)
	}
	f.Add("/a\x00")
	f.Add(`\\host\..`)
	f.Add(`\\.\share`)
	f.Fuzz(func(t *testing.T, s string) {
		for _, os := range []string{"linux", "windows"} {
			locs, e := ParsePathForOS(s, os)
			if e != nil {
				continue
			}
			var b strings.Builder
			for _, loc := range locs {
				if !validLocation(loc.Address, os) {
					t.Fatal("invalid location")
				}
				if loc.CheckOnly {
					b.WriteString("check")
					if loc.ReadOnly {
						b.WriteString(",ro")
					}
					b.WriteByte('=')
				} else if loc.ReadOnly {
					b.WriteString("ro=")
				}
				b.WriteByte('"')
				b.WriteString(strings.ReplaceAll(loc.Address, "\"", "\"\""))
				b.WriteString("\";")
			}
			again, e := ParsePathForOS(b.String(), os)
			if e != nil || !reflect.DeepEqual(locs, again) {
				t.Fatal("path round trip", e)
			}
		}
		_, _ = ParsePath(s)
	})
}
func FuzzEndpoint(f *testing.F) {
	for _, s := range endpointSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ep, e := Endpoint(s)
		if e != nil {
			return
		}
		u, e := url.Parse(ep)
		if e != nil || u.User != nil || u.Fragment != "" || !strings.HasSuffix(u.EscapedPath(), "/lock") {
			t.Fatal("invalid endpoint")
		}
		base, _ := url.Parse(s)
		if u.RawQuery != base.RawQuery {
			t.Fatal("query changed")
		}
	})
}
func FuzzQuery(f *testing.F) {
	for _, s := range []string{"", "key=raw.blake2.YWJj", "key=a+b&key=a%2520b", "key=%26", "key=%20", "%6bey=x", "%zz=x", "key=%zz", "unknown=%zz", "key=x&&key=y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		keys, e := queryKeys(s)
		if e != nil {
			return
		}
		var fields []string
		for _, key := range keys {
			fields = append(fields, "key="+strings.ReplaceAll(url.QueryEscape(key), "+", "%20"))
		}
		again, e := queryKeys(strings.Join(fields, "&"))
		if e != nil || !reflect.DeepEqual(keys, again) {
			t.Fatal("query round trip", e)
		}
	})
}

func FuzzMediaType(f *testing.F) {
	for _, s := range []string{"", "application/octet-stream", "Application/Octet-Stream", "application/octet-stream; charset=utf-8", "text/plain", "application/octet-stream; broken"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if mediaOK(s) && !mediaOK(strings.ToUpper(s)) {
			t.Fatal("case changed media type")
		}
	})
}
func FuzzWrite(f *testing.F) {
	for _, s := range valueSeeds() {
		f.Add(string(abc), s, "create")
	}
	f.Add("bad", "", "create")
	f.Add(string(abc), "", "bad")
	f.Add(string(abc), "long", "replace")
	f.Fuzz(func(t *testing.T, id, value, mode string) {
		for _, l := range []Limits{{ValueBytes: 1}, {ValueBytes: 1 << 20}} {
			w, e := validateWrite(Write{ID(id), value, Mode(mode)}, l)
			if e != nil {
				continue
			}
			if _, e = ParseID(string(w.ID)); e != nil {
				t.Fatal(e)
			}
			v, e := NormalizeValue(w.Value)
			if e != nil || v != w.Value {
				t.Fatal("write not canonical")
			}
		}
		for _, limits := range []Limits{{}, {LineBytes: 64}, {ScanBytes: 65}} {
			l, _ := limits.normalized()
			w, err := validateLocalWrite(Write{ID(id), value, Mode(mode)}, l)
			if err != nil {
				continue
			}
			var b bytes.Buffer
			if err := EncodeLeaf(&b, []Record{{w.ID, w.Value}}); err != nil {
				t.Fatal(err)
			}
			leaf, err := DecodeLeaf(&b, "", l)
			if err != nil || len(leaf.Records) != 1 || leaf.Records[0] != (Record{w.ID, w.Value}) {
				t.Fatal("accepted local record cannot be read", err)
			}
		}
	})
}
func FuzzLeafEncoder(f *testing.F) {
	f.Add(string(abc), "v", false)
	f.Add("bad", "v", false)
	f.Add(string(abc), "\x00", false)
	f.Add(string(abc), "v", true)
	f.Fuzz(func(t *testing.T, id, value string, duplicate bool) {
		records := []Record{{ID(id), value}}
		if duplicate {
			records = append(records, records[0])
		}
		var b bytes.Buffer
		e := EncodeLeaf(&b, records)
		if e != nil {
			if b.Len() != 0 {
				t.Fatal("partial invalid encoding")
			}
			return
		}
		decoded, e := DecodeLeaf(&b, "", Limits{})
		if errors.Is(e, ErrLimit) {
			return
		}
		if e != nil || len(decoded.Records) != 1 {
			t.Fatal(e)
		}
		if e = EncodeLeaf(failedIO{}, records); !errors.Is(e, io.ErrClosedPipe) {
			t.Fatal(e)
		}
	})
}

func FuzzBatchValidation(f *testing.F) {
	for _, id := range []string{string(abc), "bad"} {
		for _, value := range []string{"v", "\x00", "long"} {
			for _, mode := range []string{"create", "replace", "bad"} {
				f.Add(id, value, mode)
			}
		}
	}
	f.Fuzz(func(t *testing.T, id, value, mode string) {
		for _, count := range []int{0, 1, 2} {
			for _, put := range []bool{false, true} {
				for _, limit := range []int{1, 10} {
					req := Request{Put: put}
					for i := 0; i < count; i++ {
						req.Items = append(req.Items, Write{ID(id), value, Mode(mode)})
					}
					checked, e := validateBatch(req, Limits{Entries: limit, ValueBytes: 1})
					if e == nil {
						again, e := validateBatch(checked, Limits{Entries: limit, ValueBytes: 1})
						if e != nil || !reflect.DeepEqual(checked, again) {
							t.Fatal("unstable validation")
						}
					}
				}
			}
		}
	})
}

func FuzzShardName(f *testing.F) {
	for _, name := range []string{"AA", "A", ".AA.old", ".A.tmp", ".tmp", ".old", "..tmp", "aa", "unknown", ".AA.guard"} {
		for _, depth := range []int{0, 1, 2, 52, -1} {
			f.Add(name, depth)
		}
	}
	f.Fuzz(func(t *testing.T, name string, depth int) {
		base, side, e := shardName(name, depth)
		if e != nil {
			return
		}
		if strings.Trim(base, alphabet) != "" || len(base) < 1 || len(base) > 2 {
			t.Fatal("invalid shard")
		}
		if side != "" && side != "old" && side != "tmp" {
			t.Fatal("invalid sidecar")
		}
		again, _, e := shardName(base, depth)
		if e != nil || again != base {
			t.Fatal("unstable shard")
		}
	})
}

func FuzzLocalConfig(f *testing.F) {
	for _, path := range []string{"/tmp/locks", "/tmp/locks///", "relative", "/", "/tmp/.locks.guard", "/tmp/lo\x00cks", "/tmp/\x00/../locks"} {
		f.Add(path, 0, 0, 0)
	}
	f.Add("/tmp/locks", -1, 0, 0)
	f.Add("/tmp/locks", 0, 65535, 0)
	f.Add("/tmp/locks", 0, 0, -1)
	f.Fuzz(func(t *testing.T, path string, timeout, mode, budget int) {
		s, e := OpenLocal(path, LocalOptions{LockTimeout: time.Duration(timeout), FileMode: os.FileMode(mode), Limits: Limits{ScanBytes: budget}})
		if e != nil {
			return
		}
		if strings.ContainsRune(path, 0) || !filepath.IsAbs(s.path) || s.path != filepath.Clean(s.path) || s.path == s.parent || s.name == "" {
			t.Fatal("invalid local configuration")
		}
	})
}
