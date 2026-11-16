//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalNoParallel(t *testing.T) {
	s := testLocalNoParallel(t)
	// Ordinary clients can read the same bytes after exclusive use ends.
	ordinary, err := OpenLocal(s.path, LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	getWant(t, ordinary, testID(0), Found, "batch")
}

func newNoParallelLocal(t *testing.T) *Local {
	t.Helper()
	s := newLocal(t)
	s, err := OpenLocal(s.path, LocalOptions{NoParallel: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testLocalNoParallel(t *testing.T) *Local {
	t.Helper()
	s := newNoParallelLocal(t)
	ctx := context.Background()
	getWant(t, s, abc, Missing, "")
	putWant(t, s, abc, "old", Create, Created)
	// Simulate an interrupted publication, then read and recover the backup.
	if err := os.Rename(s.path, filepath.Join(s.parent, ".locks.old")); err != nil {
		t.Fatal(err)
	}
	getWant(t, s, abc, Found, "old")
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	putWant(t, s, abc, "new", Replace, Replaced)
	getWant(t, s, abc, Found, "new")
	out, err := s.PutBatch(ctx, []Write{{abc, "batch", Replace}})
	if err != nil || len(out) != 1 || out[0].Err != nil || out[0].Result.Status != Replaced {
		t.Fatalf("batch write: %v, %v", out, err)
	}
	// Force a root split, then exercise both batch and point shard operations.
	var data strings.Builder
	for i := 0; i < 512; i++ {
		fmt.Fprintf(&data, "%s value\n", testID(i))
	}
	writeFile(t, s.path, data.String())
	putWant(t, s, testID(512), "value", Create, Created)
	putWant(t, s, testID(0), "changed", Replace, Replaced)
	out, err = s.PutBatch(ctx, []Write{{testID(0), "batch", Replace}, {testID(513), "value", Create}})
	if err != nil || len(out) != 2 || out[0].Err != nil || out[1].Err != nil || out[0].Result.Status != Replaced || out[1].Result.Status != Created {
		t.Fatalf("shard batch write: %v, %v", out, err)
	}
	out, err = s.GetBatch(ctx, []ID{testID(0), testID(513)})
	if err != nil || len(out) != 2 || out[0].Err != nil || out[1].Err != nil || out[0].Result.Value != "batch" || out[1].Result.Value != "value" {
		t.Fatalf("shard batch read: %v, %v", out, err)
	}
	getWant(t, s, testID(0), Found, "batch")
	if err := s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNoParallelMalformedData(t *testing.T) {
	s := newNoParallelLocal(t)
	putWant(t, s, abc, "value", Create, Created)
	writeFile(t, s.path, "<<<<<<< unresolved\n")
	ctx := context.Background()
	_, err := s.Get(ctx, abc)
	requireErr(t, err, ErrFormat)
	_, err = s.Put(ctx, abc, "changed", Replace)
	requireErr(t, err, ErrFormat)
	requireErr(t, s.Recover(ctx), ErrFormat)
	data, err := os.ReadFile(s.path)
	if err != nil || string(data) != "<<<<<<< unresolved\n" {
		t.Fatalf("malformed data changed: %q, %v", data, err)
	}
}

func TestNoParallelConcurrentCreate(t *testing.T) {
	for _, sharded := range []bool{false, true} {
		t.Run(fmt.Sprintf("sharded=%t", sharded), func(t *testing.T) {
			s := newNoParallelLocal(t)
			peer, err := OpenLocal(s.path, LocalOptions{NoParallel: true})
			if err != nil {
				t.Fatal(err)
			}
			if sharded {
				if err := os.Mkdir(s.path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			results := make(chan Outcome, 24)
			for i := 0; i < cap(results); i++ {
				go func(i int) {
					<-start
					if i%2 == 0 {
						r, err := s.Put(context.Background(), abc, fmt.Sprint(i), Create)
						results <- Outcome{Result: r, Err: err}
					} else {
						out, err := peer.PutBatch(context.Background(), []Write{{abc, fmt.Sprint(i), Create}})
						if err != nil {
							results <- Outcome{Err: err}
						} else {
							results <- out[0]
						}
					}
				}(i)
			}
			close(start)
			created := 0
			for i := 0; i < cap(results); i++ {
				out := <-results
				if out.Err != nil {
					t.Fatal(out.Err)
				}
				switch out.Result.Status {
				case Created:
					created++
				case Conflict:
				default:
					t.Fatal(out)
				}
			}
			if created != 1 {
				t.Fatalf("created %d entries; want 1", created)
			}
		})
	}
}

func TestNoParallelGuardDeadline(t *testing.T) {
	s := newNoParallelLocal(t)
	peer, err := OpenLocal(s.path, LocalOptions{NoParallel: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := openParent(context.Background(), s.parent)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	g, err := s.getGuard(d, true)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	if err := g.lock(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	defer g.unlock(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = peer.Get(ctx, abc)
	requireErr(t, err, context.DeadlineExceeded)
	_, err = getGuard(d, ".locks.guard", false)
	requireErr(t, err, ErrFormat)
}

func TestNoParallelConflictsWithFileLockMode(t *testing.T) {
	s := newNoParallelLocal(t)
	d, err := openParent(context.Background(), s.parent)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	g, err := getGuard(d, ".locks.guard", true)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	_, err = s.Put(context.Background(), abc, "value", Create)
	requireErr(t, err, ErrFormat)
	if _, err = os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatalf("conflicting mode changed store: %v", err)
	}
}
