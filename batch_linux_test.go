//go:build linux

package locks

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalBatchSplitsAndScans(t *testing.T) {
	s := newLocal(t)
	ctx := context.Background()
	var writes []Write
	var ids []ID
	for i := 0; i < 600; i++ {
		id := testID(i)
		ids = append(ids, id)
		writes = append(writes, Write{id, "v", Create})
	}
	out, e := s.PutBatch(ctx, writes)
	if e != nil || len(out) != 600 {
		t.Fatal(e)
	}
	for _, o := range out {
		if o.Err != nil || o.Result.Status != Created {
			t.Fatal(o)
		}
	}
	if e = s.Validate(ctx); e != nil {
		t.Fatal(e)
	}
	original := system
	defer func() { system = original }()
	opens := map[string]int{}
	system.Openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		opens[name]++
		return original.Openat(fd, name, flags, mode)
	}
	out, e = s.GetBatch(ctx, ids)
	system = original
	if e != nil || len(out) != 600 {
		t.Fatal(e)
	}
	for _, o := range out {
		if o.Err != nil || o.Result.Status != Found {
			t.Fatal(o)
		}
	}
	for name, n := range opens {
		if len(name) == 2 && strings.Trim(name, alphabet) == "" && n != 1 {
			t.Fatalf("leaf %s opened %d times", name, n)
		}
	}
	out, e = s.PutBatch(ctx, []Write{{ids[0], "new", Replace}, {ids[1], "new", Replace}, {testID(10000), "new", Create}})
	if e != nil {
		t.Fatal(e)
	}
	for _, o := range out {
		if o.Err != nil {
			t.Fatal(o)
		}
	}
	// Recovery of root sidecars must release exclusive root ownership and use slots.
	writeFile(t, filepath.Join(s.parent, ".locks.old"), string(abc)+" old\n")
	out, e = s.PutBatch(ctx, []Write{{ids[0], "new", Create}})
	if e != nil || out[0].Err != nil {
		t.Fatal(out, e)
	}
}
