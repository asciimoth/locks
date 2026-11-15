//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalBatches(t *testing.T) {
	s := newLocal(t)
	ctx := context.Background()
	ids := []ID{abc, testID(1), testID(2)}
	out, e := s.GetBatch(ctx, ids)
	if e != nil || len(out) != 3 {
		t.Fatal(out, e)
	}
	for _, o := range out {
		if o.Err != nil || o.Result.Status != Missing {
			t.Fatal(o)
		}
	}
	items := []Write{{abc, "first\r", Create}, {ids[1], "second", Replace}, {ids[2], "", Create}}
	out, e = s.PutBatch(ctx, items)
	if e != nil || len(out) != 3 {
		t.Fatal(out, e)
	}
	for _, o := range out {
		if o.Err != nil || o.Result.Status != Created {
			t.Fatal(o)
		}
	}
	if items[0].Value != "first\r" {
		t.Fatal("mutated caller input")
	}
	out, e = s.PutBatch(ctx, []Write{{abc, "different", Create}, {ids[1], "new", Replace}, {ids[2], "", Create}})
	if e != nil || out[0].Result.Status != Conflict || out[1].Result.Status != Replaced || out[2].Result.Status != Same {
		t.Fatal(out, e)
	}
	out, e = s.GetBatch(ctx, ids)
	if e != nil || out[0].Result.Value != "first" || out[1].Result.Value != "new" {
		t.Fatal(out, e)
	}
	for _, bad := range [][]Write{nil, {{ID: "bad"}}, {{abc, "", Create}, {abc, "", Create}}, {{abc, "", Create}, {ids[1], "\x00", Create}}} {
		_, e = s.PutBatch(ctx, bad)
		requireErr(t, e, ErrFormat)
	}
	small, _ := OpenLocal(s.path, LocalOptions{Limits: Limits{Entries: 1}})
	_, e = small.GetBatch(ctx, ids)
	requireErr(t, e, ErrLimit)
	_, e = s.GetBatch(ctx, nil)
	requireErr(t, e, ErrFormat)
	ro, _ := OpenLocal(s.path, LocalOptions{ReadOnly: true})
	_, e = ro.PutBatch(ctx, items)
	requireErr(t, e, ErrDenied)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, e = s.GetBatch(canceled, ids)
	requireErr(t, e, context.Canceled)
}
func TestLocalBatchCommentsAndRecovery(t *testing.T) {
	s := newLocal(t)
	ctx := context.Background()
	writeFile(t, s.path, "# ignored\r\n"+string(abc)+" old\r\n# final")
	out, e := s.PutBatch(ctx, []Write{{abc, "new", Replace}, {testID(1), "one", Create}, {testID(2), "two", Create}})
	if e != nil {
		t.Fatal(e)
	}
	for _, o := range out {
		if o.Err != nil {
			t.Fatal(o)
		}
	}
	b, _ := os.ReadFile(s.path)
	want := "# ignored\r\n" + string(abc) + " new\n# final\n" + string(testID(1)) + " one\n" + string(testID(2)) + " two\n"
	if string(b) != want {
		t.Fatalf("%q", b)
	}
	_ = os.Rename(s.path, filepath.Join(s.parent, ".locks.old"))
	out, e = s.GetBatch(ctx, []ID{abc, testID(1)})
	if e != nil || out[0].Result.Value != "new" {
		t.Fatal(out, e)
	}
	// Unguarded sidecars are an error, including on a batch read.
	_ = os.Remove(filepath.Join(s.parent, ".locks.guard"))
	out, e = s.GetBatch(ctx, []ID{abc})
	if e != nil || !errors.Is(out[0].Err, ErrFormat) {
		t.Fatal(out, e)
	}
}
func TestLocalBatchPartialFailures(t *testing.T) {
	s := newLocal(t)
	_ = os.Mkdir(s.path, 0700)
	writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
	writeFile(t, filepath.Join(s.path, "XX"), "BAD\n")
	other := testID(1)
	ctx := context.Background()
	out, e := s.PutBatch(ctx, []Write{{abc, "v", Create}, {other, "v", Create}})
	if e != nil || out[0].Err == nil || out[1].Result.Status != Created {
		t.Fatal(out, e)
	}
	d, e := openParent(context.Background(), s.parent)
	if e != nil {
		t.Fatal(e)
	}
	defer d.f.Close()
	g, e := getGuard(d, ".locks.guard", true)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	if e = g.lock(ctx, 0, false); e != nil {
		t.Fatal(e)
	}
	defer g.unlock(0)
	if e = g.lock(ctx, slotFor(abc), true); e != nil {
		t.Fatal(e)
	}
	defer g.unlock(slotFor(abc))
	// Leave time to open the parent and finish the independent shard even on
	// slower filesystems. Only the held slot should exhaust this deadline.
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, e = s.GetBatch(short, []ID{other, abc})
	if e != nil || out[0].Result.Status != Found || !errors.Is(out[1].Err, context.DeadlineExceeded) {
		t.Fatal(out, e)
	}
}
func TestHTTPWithLocalBatch(t *testing.T) {
	s := newLocal(t)
	server := httptest.NewServer(testHandler(t, s, HandlerOptions{}))
	defer server.Close()
	client, _ := OpenHTTP(server.URL, HTTPOptions{})
	out, e := client.PutBatch(context.Background(), []Write{{abc, "a", Create}, {testID(1), "b", Create}})
	if e != nil || out[0].Err != nil || out[1].Err != nil {
		t.Fatal(out, e)
	}
	getWant(t, client, abc, Found, "a")
	readonly, _ := OpenLocal(s.path, LocalOptions{ReadOnly: true})
	roserver := httptest.NewServer(testHandler(t, readonly, HandlerOptions{}))
	defer roserver.Close()
	client, _ = OpenHTTP(roserver.URL, HTTPOptions{})
	_, e = client.Put(context.Background(), abc, "c", Replace)
	requireErr(t, e, ErrDenied)
}
