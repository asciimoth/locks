//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Build the same metadata on directories and leaves that a library split uses.
func seedMergeTree(t *testing.T, s *Local, records []Record, depth int) {
	t.Helper()
	seed := filepath.Join(s.parent, "seed")
	writeHealthLeaf(t, seed, nil)
	d, err := openParent(context.Background(), s.parent)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	source, err := d.open("seed", false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		t.Fatal(err)
	}
	mode := uint32(info.Mode().Perm())
	var build func(*directory, string, string, []Record, int)
	build = func(d *directory, name, p string, records []Record, depth int) {
		if depth == 0 {
			if err := s.stage(d, name, p, nil, records, source, mode); err != nil {
				t.Fatal(err)
			}
			return
		}
		sub, err := d.createDir(name)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.f.Close()
		groups := map[string][]Record{}
		for _, record := range records {
			c := component(prefix(record.ID), len(p))
			groups[c] = append(groups[c], record)
		}
		for c, group := range groups {
			build(sub, c, p+c, group, depth-1)
		}
		if err := setMetadata(source, sub.f, mode|((mode&0444)>>2), true); err != nil {
			t.Fatal(err)
		}
	}
	build(d, s.name, "", records, depth)
}

func mergeRepair(s *Local) error {
	return s.RebalanceWithOptions(context.Background(), RebalanceOptions{Merge: true})
}

func TestMergeSmallTrees(t *testing.T) {
	for _, n := range []int{0, 1, 512, 513} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, path := healthStore(t)
			records := healthRecords(t, n)
			seedMergeTree(t, s, records, 2)
			if err := s.Rebalance(context.Background()); err != nil {
				t.Fatal(err)
			}
			st, _ := os.Stat(path)
			if !st.IsDir() {
				t.Fatal("merged without opt-in")
			}
			if err := mergeRepair(s); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil || st.IsDir() != (n > 512) {
				t.Fatal(st, err)
			}
			h, err := s.Inspect(context.Background())
			if err != nil || h.Records != n || h.Pending != 0 || h.Overfull != 0 {
				t.Fatal(h, err)
			}
			if n <= 512 && h.Leaves != 1 {
				t.Fatal(h)
			}
			for _, record := range records {
				r, err := s.Get(context.Background(), record.ID)
				if err != nil || r.Status != Found || r.Value != record.Value {
					t.Fatal(r, err)
				}
			}
			if err := mergeRepair(s); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMergeOrderAndLimits(t *testing.T) {
	s, path := healthStore(t)
	records := healthRecords(t, 20)
	// Unfamiliar algorithms and empty values must survive merging.
	records[0].ID = ID(strings.Split(string(records[0].ID), ":")[0] + ":future")
	records[0].Value = ""
	seedMergeTree(t, s, records, 1)
	first := filepath.Join(path, string(records[0].ID)[:2])
	f, err := os.OpenFile(first, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("# comment\n")
	if err = errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
	if err := mergeRepair(s); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := DecodeLeaf(f, "", Limits{})
	if err = errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(records, func(i, j int) bool { return string(records[i].ID)[:2] < string(records[j].ID)[:2] })
	for i, record := range records {
		if leaf.Records[i] != record {
			t.Fatal("record order changed", i)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "# comment") {
		t.Fatal(string(data), err)
	}

	s, path = healthStore(t)
	records = healthRecords(t, 2)
	seedMergeTree(t, s, records, 2)
	s.limits.ScanBytes = 100
	if err := mergeRepair(s); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		t.Fatal("merged beyond scan budget", st, err)
	}
	// A child that exceeds the budget must also prevent its parent from merging.
	s, path = healthStore(t)
	for i := range records {
		records[i].ID = ID("AA" + string(records[i].ID)[2:])
	}
	seedMergeTree(t, s, records, 2)
	s.limits.ScanBytes = 100
	if err := mergeRepair(s); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(filepath.Join(path, "AA"))
	if err != nil || !st.IsDir() {
		t.Fatal(st, err)
	}
}

func TestMergeFailuresPreserveData(t *testing.T) {
	for _, kind := range []string{"hardlink", "malformed", "unknown", "canceled", "readonly", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s, path := healthStore(t)
			records := healthRecords(t, 2)
			seedMergeTree(t, s, records, 1)
			leaf := filepath.Join(path, string(records[0].ID)[:2])
			switch kind {
			case "hardlink":
				if err := os.Link(leaf, filepath.Join(s.parent, "alias")); err != nil {
					t.Skip(err)
				}
			case "malformed":
				if err := os.WriteFile(leaf, []byte("BAD\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if err := os.WriteFile(filepath.Join(path, "unknown"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "readonly":
				s.options.ReadOnly = true
			case "missing":
				s.parent = filepath.Join(s.parent, "missing")
			}
			ctx := context.Background()
			if kind == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := s.RebalanceWithOptions(ctx, RebalanceOptions{Merge: true}); err == nil {
				t.Fatal("expected error")
			}
			st, err := os.Stat(path)
			if err != nil || !st.IsDir() {
				t.Fatal(st, err)
			}
		})
	}
	var s *Local
	if err := mergeRepair(s); !errors.Is(err, ErrFormat) {
		t.Fatal(err)
	}
}

func TestMergeRecoveryStates(t *testing.T) {
	for _, phase := range []string{"staging", "parked", "installed", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			s, path := healthStore(t)
			records := healthRecords(t, 2)
			seedMergeTree(t, s, records, 1)
			if err := s.Validate(context.Background()); err != nil {
				t.Fatal(err)
			}
			old, tmp := filepath.Join(s.parent, ".locks.old"), filepath.Join(s.parent, ".locks.tmp")
			writeHealthLeaf(t, old, records)
			if phase == "staging" {
				if err := os.WriteFile(old, []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(path, tmp); err != nil {
					t.Fatal(err)
				}
				if phase != "parked" {
					if err := os.Rename(old, path); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "cleanup" {
					if err := os.Remove(filepath.Join(tmp, string(records[0].ID)[:2])); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, record := range records {
				r, err := s.Get(context.Background(), record.ID)
				if err != nil || r.Status != Found || r.Value != record.Value {
					t.Fatal(r, err)
				}
			}
			out, err := s.GetBatch(context.Background(), []ID{records[0].ID, records[1].ID})
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range out {
				if result.Err != nil || result.Result.Status != Found {
					t.Fatal(result)
				}
			}
			if err := s.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				r, err := s.Get(context.Background(), record.ID)
				if err != nil || r.Status != Found || r.Value != record.Value {
					t.Fatal(r, err)
				}
			}
			for _, side := range []string{old, tmp} {
				if _, err := os.Stat(side); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(side, err)
				}
			}
		})
	}
}
