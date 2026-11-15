//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func FuzzLocalLeaf(f *testing.F) {
	for _, data := range leafSeeds() {
		f.Add(data, false, false)
		f.Add(data, true, true)
	}
	f.Fuzz(func(t *testing.T, data string, sharded, backup bool) {
		if len(data) > 4096 {
			t.Skip("bounded filesystem fixture")
		}
		s := newLocal(t)
		ctx := context.Background()
		path, p := s.path, ""
		if sharded {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			p = string(abc)[:2]
			path = filepath.Join(path, p)
		}
		if backup {
			writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
			path = filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".old")
		}
		writeFile(t, path, data)
		leaf, decodeErr := DecodeLeaf(strings.NewReader(data), p, Limits{})
		result, err := s.Get(ctx, abc)
		out, batchErr := s.GetBatch(ctx, []ID{abc})
		if batchErr != nil || len(out) != 1 {
			t.Fatal("batch read failed", batchErr)
		}
		if decodeErr != nil {
			if err == nil || out[0].Err == nil || s.Validate(ctx) == nil || s.Recover(ctx) == nil {
				t.Fatal("accepted malformed leaf")
			}
			if _, err := s.Put(ctx, abc, "new", Replace); err == nil {
				t.Fatal("overwrote malformed leaf")
			}
			writes, err := s.PutBatch(ctx, []Write{{abc, "new", Replace}})
			if err == nil && (len(writes) != 1 || writes[0].Err == nil) {
				t.Fatal("batch overwrote malformed leaf")
			}
			remaining, err := os.ReadFile(path)
			if err != nil || string(remaining) != data {
				t.Fatal("changed malformed evidence", err)
			}
			return
		}
		want := Result{ID: abc, Status: Missing}
		for _, record := range leaf.Records {
			if record.ID == abc {
				want.Status, want.Value = Found, record.Value
			}
		}
		if err != nil || result != want || out[0].Err != nil || out[0].Result != want {
			t.Fatal("local reads differ from decoded leaf", result, out, err)
		}
		if err := s.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(ctx, abc, "new", Replace); err != nil {
			t.Fatal(err)
		}
		getWant(t, s, abc, Found, "new")
	})
}

func FuzzLocalShardNames(f *testing.F) {
	for _, name := range []string{"AA", "A", "aa", ".AA.old", ".AA.tmp", ".tmp", ".old", "..tmp", ".AA.guard", "\xff", "A\ufffd", "# comment"} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		// Keep generated entries inside the test directory. Windows rejects
		// some names that POSIX filesystems permit.
		if name == "" || name == "." || name == ".." || len(name) > 128 || strings.ContainsAny(name, "/\\\x00") ||
			runtime.GOOS == "windows" && !validWindowsName(name) {
			t.Skip("not a portable child name")
		}
		s := newLocal(t)
		if err := os.Mkdir(s.path, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.path, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Skipf("filesystem rejected name: %v", err)
		}
		ctx := context.Background()
		_, _, nameErr := shardName(name, 0)
		validateErr, recoverErr := s.Validate(ctx), s.Recover(ctx)
		if nameErr != nil {
			if validateErr == nil || recoverErr == nil {
				t.Fatal("accepted invalid shard name")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("removed unrecognized entry", err)
			}
		} else if validateErr != nil || recoverErr != nil {
			t.Fatal("rejected valid shard name", validateErr, recoverErr)
		}
		// A point read may ignore unrelated entries, but must not panic.
		_, _ = s.Get(ctx, abc)
		_, _ = s.GetBatch(ctx, []ID{abc})
	})
}
