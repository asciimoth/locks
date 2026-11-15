//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func healthStore(t *testing.T) (*Local, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "locks")
	s, err := OpenLocal(path, LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func healthRecords(t *testing.T, n int) []Record {
	t.Helper()
	records := make([]Record, n)
	for i := range records {
		id, err := Hash([]byte(fmt.Sprint(i)), "")
		if err != nil {
			t.Fatal(err)
		}
		records[i] = Record{id, fmt.Sprint(i)}
	}
	return records
}

func writeHealthLeaf(t *testing.T, path string, records []Record) {
	t.Helper()
	var b strings.Builder
	if err := EncodeLeaf(&b, records); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInspectAndRebalance(t *testing.T) {
	s, path := healthStore(t)
	ctx := context.Background()
	h, err := s.Inspect(ctx)
	if err != nil || h != (Health{}) {
		t.Fatal(h, err)
	}
	records := healthRecords(t, 513)
	writeHealthLeaf(t, path, records)
	h, err = s.Inspect(ctx)
	if err != nil || h != (Health{Leaves: 1, Records: 513, Largest: 513, Overfull: 1}) {
		t.Fatal(h, err)
	}
	if err := s.Rebalance(ctx); err != nil {
		t.Fatal(err)
	}
	h, err = s.Inspect(ctx)
	if err != nil || h.Records != 513 || h.Leaves < 2 || h.Largest > 512 || h.Overfull != 0 || h.Pending != 0 {
		t.Fatal(h, err)
	}
	for _, record := range records {
		r, err := s.Get(ctx, record.ID)
		if err != nil || r.Value != record.Value {
			t.Fatal(r, err)
		}
	}
	if err := s.Rebalance(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.Inspect(ctx)
	if err != nil || after != h {
		t.Fatal(after, err)
	}
}

func TestHealthRecoveryAndCorruption(t *testing.T) {
	ctx := context.Background()
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			s, path := healthStore(t)
			records := healthRecords(t, 1)
			node := path
			if nested {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				node = filepath.Join(path, string(records[0].ID)[:2])
			}
			old := filepath.Join(filepath.Dir(node), "."+filepath.Base(node)+".old")
			tmp := filepath.Join(filepath.Dir(node), "."+filepath.Base(node)+".tmp")
			writeHealthLeaf(t, old, records)
			if err := os.WriteFile(tmp, []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
			h, err := s.Inspect(ctx)
			if err != nil || h.Pending != 2 || h.Records != 1 {
				t.Fatal(h, err)
			}
			if err := os.WriteFile(node, []byte("<<<<<<< ours\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Rebalance(ctx); !errors.Is(err, ErrFormat) {
				t.Fatal(err)
			}
			if _, err := os.Stat(old); err != nil {
				t.Fatal("lost backup", err)
			}
			if err := os.Remove(node); err != nil {
				t.Fatal(err)
			}
			if err := s.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
			h, err = s.Inspect(ctx)
			if err != nil || h.Pending != 0 || h.Records != 1 {
				t.Fatal(h, err)
			}
		})
	}
}

func TestHealthInputAndLock(t *testing.T) {
	ctx := context.Background()
	for _, s := range []*Local{nil, {}} {
		if _, err := s.Inspect(ctx); !errors.Is(err, ErrFormat) {
			t.Fatal(err)
		}
		if err := s.Rebalance(ctx); !errors.Is(err, ErrFormat) {
			t.Fatal(err)
		}
	}
	s, _ := healthStore(t)
	if _, err := s.Inspect(nil); !errors.Is(err, ErrFormat) {
		t.Fatal(err)
	}
	s.options.ReadOnly = true
	if err := s.Rebalance(ctx); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	s.options.ReadOnly = false
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Inspect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Hold byte 0 to verify that inspection and repair use the normal guard.
	ctx = context.Background()
	d, err := openParent(ctx, s.parent)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	g, err := getGuard(d, "."+s.name+".guard", true)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	if err := g.lock(ctx, 0, true); err != nil {
		t.Fatal(err)
	}
	defer g.unlock(0)
	deadline, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err := s.Rebalance(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestRebalanceNestedAndTerminal(t *testing.T) {
	s, path := healthStore(t)
	records := healthRecords(t, 513)
	// Route all records into one first-level shard to exercise a deeper split.
	for i := range records {
		records[i].ID = ID("AA" + string(records[i].ID)[2:])
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	writeHealthLeaf(t, filepath.Join(path, "AA"), records)
	if err := s.Rebalance(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := s.Inspect(context.Background())
	if err != nil || h.Records != 513 || h.Overfull != 0 {
		t.Fatal(h, err)
	}
	// A terminal leaf may remain overfull. Distinct algorithms retain identity.
	s, path = healthStore(t)
	digest := strings.Repeat("A", 52)
	node := filepath.Join(path, "AA")
	for i := 2; i < 52; i++ {
		node = filepath.Join(node, "A")
	}
	if err := os.MkdirAll(filepath.Dir(node), 0700); err != nil {
		t.Fatal(err)
	}
	for i := range records {
		records[i].ID = ID(fmt.Sprintf("%s:test%d", digest, i))
	}
	writeHealthLeaf(t, node, records)
	if err := s.Rebalance(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err = s.Inspect(context.Background())
	if err != nil || h.Overfull != 0 || h.Largest != 513 {
		t.Fatal(h, err)
	}
}

func TestRebalancePreservesInvalidTree(t *testing.T) {
	s, path := healthStore(t)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	records := healthRecords(t, 513)
	for i := range records {
		records[i].ID = ID("AA" + string(records[i].ID)[2:])
	}
	leaf := filepath.Join(path, "AA")
	writeHealthLeaf(t, leaf, records)
	before, err := os.ReadFile(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "AB"), []byte("BAD\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebalance(context.Background()); !errors.Is(err, ErrFormat) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(leaf)
	if err != nil || string(before) != string(after) {
		t.Fatal("valid sibling changed before validation", err)
	}
}

func TestRebalancePreservesHardLinks(t *testing.T) {
	s, path := healthStore(t)
	writeHealthLeaf(t, path, healthRecords(t, 513))
	alias := filepath.Join(filepath.Dir(path), "alias")
	if err := os.Link(path, alias); err != nil {
		t.Skip("hard links unavailable", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rebalance(context.Background()); !errors.Is(err, ErrFormat) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("hard-linked leaf changed", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".locks.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staged hard-linked leaf", err)
	}
}
