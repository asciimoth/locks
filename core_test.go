package locks

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

var abc ID = "XXOYCPDDII4XEMLR54765GCXTOKJMTR3WHFT4QTSMLEMA2GVEMMQ:blake2"

func testID(i int) ID { id, _ := Hash([]byte(fmt.Sprint(i)), "blake2"); return id }
func requireErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
func TestHashVectors(t *testing.T) {
	for _, tt := range []struct{ raw, alg, want string }{
		{"", "blake2", "BZLVDQBG4VB3F2FLF2YGBGO2UHI6LX2HO6HXPB72VNC434JP4OUA:blake2"},
		{"abc", "blake2", string(abc)},
		{"\x00\n\xff", "blake2", "FYXQGQ4JSJ7PU5XKXCEGMLN66EB6X6I2XRHRLVOBCOS2HNWW55FQ:blake2"},
		{"abc", "sha256", "XJ4BNP4PAHH6UQKBIDPF3LRCEOYAGYNDSYLXVHFUCD7WD4QACWWQ:sha256"},
		{"abc", "", string(abc)},
	} {
		id, e := Hash([]byte(tt.raw), tt.alg)
		if e != nil || string(id) != tt.want {
			t.Fatalf("Hash: %s, %v", id, e)
		}
	}
	_, e := Hash(nil, "unknown")
	requireErr(t, e, ErrUnsupported)
}
func idSeeds() []string {
	return []string{"", string(abc), strings.Replace(string(abc), "blake2", "future-1", 1), strings.ToLower(string(abc)), strings.Repeat("A", 52) + ":sha256", strings.Repeat("A", 52) + ":x", strings.Repeat("A", 52) + ":a" + strings.Repeat("b", 31), strings.Repeat("A", 52) + ":a" + strings.Repeat("b", 32), strings.Repeat("A", 52) + ":1bad", strings.Repeat("A", 52) + ":BAD", strings.Repeat("A", 52) + ":a_b", strings.Repeat("A", 52) + ":a:b", strings.Repeat("A", 51) + "B:blake2", strings.Repeat("A", 52) + "=:blake2", b32.EncodeToString(make([]byte, 64)) + ":future", b32.EncodeToString(make([]byte, 64)) + ":blake2", b32.EncodeToString(make([]byte, 31)) + ":future", b32.EncodeToString(make([]byte, 65)) + ":future", strings.Repeat("!", 52) + ":blake2"}
}
func TestIDValidation(t *testing.T) {
	for _, s := range idSeeds() {
		id, e := ParseID(s)
		if e == nil {
			if string(id) != s {
				t.Fatal("changed identity")
			}
			parts := strings.Split(s, ":")
			b, _ := b32.DecodeString(parts[0])
			if len(b) < 32 || len(b) > 64 {
				t.Fatal("bad size")
			}
		}
	}
	for _, s := range []string{strings.Repeat("A", 51) + "B:blake2", strings.ToLower(string(abc)), b32.EncodeToString(make([]byte, 64)) + ":sha256"} {
		_, e := ParseID(s)
		requireErr(t, e, ErrFormat)
	}
}
func valueSeeds() []string {
	return []string{"", "text", "\r\r", " a\rb\t \r\r", "\x00", "a\nb", "\xff", "é", "é"}
}
func TestValues(t *testing.T) {
	for _, s := range valueSeeds() {
		v, e := NormalizeValue(s)
		if strings.ContainsAny(s, "\x00\n") || s == "\xff" {
			requireErr(t, e, ErrFormat)
		} else if e != nil || v != strings.TrimRight(s, "\r") {
			t.Fatalf("normalize %q: %q %v", s, v, e)
		}
	}
}
func keySeeds() []string {
	return []string{"id." + string(abc), "raw.blake2.YWJj", "raw.blake2.", "raw.sha256.AAr_", "raw.blake2.YR", "raw.blake2.YQ==", "raw.blake2.YQ\n", "raw.blake2.+/", "raw.nope.YQ", "raw.Bad.YQ", "raw.blake2", "bad", "id.bad", "raw.blake2." + base64.RawURLEncoding.EncodeToString([]byte{255, 0, 10})}
}
func TestKeys(t *testing.T) {
	id, e := ParseKey("raw.blake2.YWJj", Limits{})
	if e != nil || id != abc {
		t.Fatal(id, e)
	}
	for _, s := range keySeeds() {
		_, _ = ParseKey(s, Limits{})
	}
	_, e = ParseKey("raw.blake2.YWJj", Limits{RawKeyBytes: 2})
	requireErr(t, e, ErrLimit)
	_, e = ParseKey("raw.blake2.YWJj", Limits{RawKeyBytes: -1})
	requireErr(t, e, ErrLimit)
}

type failedIO struct{}

func (failedIO) Read([]byte) (int, error)  { return 0, io.ErrUnexpectedEOF }
func (failedIO) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func leafSeeds() []string {
	return []string{"", "# comment\n", " \n", string(abc) + " \n", string(abc) + "  x \r\n", string(abc) + " a\rb\r\r\n", string(abc) + " x", string(abc) + "\tx\n", string(abc) + " \x00\n", string(abc) + " v\n" + string(abc) + " v\n", "\xef\xbb\xbf# bom\n", "# \xff\n", "<<<<<<< branch\n", "||||||| base\n", "=======\n", ">>>>>>>>> branch\n", "=======x\n", "<<<<<< x\n", "# <<<<<<<\n", "BAD x\n", "A\n"}
}
func TestLeafVectors(t *testing.T) {
	for _, tt := range []struct{ suffix, want string }{{" \n", ""}, {"  x \r\n", " x "}, {" a\rb\r\r\n", "a\rb"}, {" x", "x"}} {
		l, e := DecodeLeaf(strings.NewReader(string(abc)+tt.suffix), "XX", Limits{})
		if e != nil || len(l.Records) != 1 || l.Records[0].Value != tt.want {
			t.Fatal(l, e)
		}
	}
	for _, s := range leafSeeds() {
		_, _ = DecodeLeaf(strings.NewReader(s), "", Limits{})
	}
	for _, p := range []string{"bad", strings.Repeat("A", 53), "XY"} {
		_, e := DecodeLeaf(strings.NewReader(string(abc)+" v\n"), p, Limits{})
		requireErr(t, e, ErrFormat)
	}
	for _, l := range []Limits{{ScanBytes: -1}, {ScanBytes: 1}, {LineBytes: 1}, {ValueBytes: 1}} {
		_, e := DecodeLeaf(strings.NewReader(string(abc)+" value\n"), "", l)
		requireErr(t, e, ErrLimit)
	}
	_, e := DecodeLeaf(failedIO{}, "", Limits{})
	requireErr(t, e, io.ErrUnexpectedEOF)
}
func TestLeafEdits(t *testing.T) {
	original := "# comment\r\n" + string(abc) + " old\r\n# unterminated"
	l, e := DecodeLeaf(strings.NewReader(original), "", Limits{})
	if e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		w      Write
		status Status
		want   string
	}{{Write{abc, "old", Create}, Same, ""}, {Write{abc, "new", Create}, Conflict, ""}, {Write{abc, "new", Replace}, Replaced, "# comment\r\n" + string(abc) + " new\n# unterminated"}, {Write{testID(1), "new", Create}, Created, original + "\n" + string(testID(1)) + " new\n"}} {
		r, b, _ := l.edit(tt.w)
		if r.Status != tt.status || string(b) != tt.want {
			t.Fatalf("%s %q", r.Status, b)
		}
	}
	var b bytes.Buffer
	if e = EncodeLeaf(&b, []Record{{abc, "v\r"}}); e != nil || b.String() != string(abc)+" v\n" {
		t.Fatal(e, b.String())
	}
	for _, rs := range [][]Record{{{ID: "bad"}}, {{abc, "\x00"}}, {{abc, "a"}, {abc, "a"}}} {
		requireErr(t, EncodeLeaf(io.Discard, rs), ErrFormat)
	}
	requireErr(t, EncodeLeaf(failedIO{}, []Record{{abc, "v"}}), io.ErrClosedPipe)
}
func TestMemory(t *testing.T) {
	m := &Memory{}
	ctx := context.Background()
	r, e := m.Get(ctx, abc)
	if e != nil || r.Status != Missing {
		t.Fatal(r, e)
	}
	for _, tt := range []struct {
		v string
		m Mode
		s Status
	}{{"", Create, Created}, {"\r", Create, Same}, {"x", Create, Conflict}, {"x", Replace, Replaced}, {"x", Replace, Same}} {
		r, e = m.Put(ctx, abc, tt.v, tt.m)
		if e != nil || r.Status != tt.s {
			t.Fatal(r, e)
		}
	}
	r, e = m.Get(ctx, abc)
	if e != nil || r.Value != "x" || r.Status != Found {
		t.Fatal(r, e)
	}
	for _, w := range []Write{{"bad", "", Create}, {abc, "", Mode("bad")}, {abc, "\x00", Create}, {abc, strings.Repeat("x", (1<<20)+1), Create}} {
		_, e = m.Put(ctx, w.ID, w.Value, w.Mode)
		if e == nil {
			t.Fatal("accepted invalid write")
		}
	}
	_, e = m.Get(ctx, "bad")
	requireErr(t, e, ErrFormat)
	c, cancel := context.WithCancel(ctx)
	cancel()
	_, e = m.Get(c, abc)
	requireErr(t, e, context.Canceled)
	_, e = m.Put(c, abc, "", Create)
	requireErr(t, e, context.Canceled)
}

func getWant(t *testing.T, s Store, id ID, status Status, value string) {
	t.Helper()
	r, e := s.Get(context.Background(), id)
	if e != nil || r.Status != status || r.Value != value {
		t.Fatalf("Get %s: %+v %v, want %s %q", id, r, e, status, value)
	}
}
func putWant(t *testing.T, s Store, id ID, value string, mode Mode, status Status) {
	t.Helper()
	r, e := s.Put(context.Background(), id, value, mode)
	if e != nil || r.Status != status {
		t.Fatalf("Put %s: %+v %v, want %s", id, r, e, status)
	}
}
