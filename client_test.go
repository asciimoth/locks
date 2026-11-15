package locks

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestClientLocal(t *testing.T) {
	if !localSupported {
		t.Skip("local storage unsupported")
	}
	t.Setenv("LOCK_PATH", "")
	root := filepath.Join(clientTempDir(t), "project", ".lock", "locks")
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A missing parent remains a backend error on reads.
	if r := c.Get(ctx, "key"); r.Err == nil {
		t.Fatal(r)
	}
	for _, tt := range []struct {
		value string
		mode  Mode
		want  Status
	}{
		{"blue\r\r", Create, Created}, {"blue", Create, Same}, {"red", Create, Conflict}, {"red", Replace, Replaced},
	} {
		r := c.Put(ctx, "key", tt.value, tt.mode)
		if r.Err != nil || r.Result.Status != tt.want {
			t.Fatal(r)
		}
	}
	if r := c.Get(ctx, "key"); r.Err != nil || r.Result.Value != "red" {
		t.Fatal(r)
	}
	if r := c.Get(ctx, "absent"); r.Err != nil || r.Result.Status != Missing {
		t.Fatal(r)
	}
	if r := c.Create(ctx, "", ""); r.Err != nil || r.Result.Status != Created {
		t.Fatal(r)
	}
	if r := c.Get(ctx, ""); r.Err != nil || r.Result.Status != Found || r.Result.Value != "" {
		t.Fatal(r)
	}
	if r := c.Replace(ctx, "", "new"); r.Err != nil || r.Result.Status != Replaced {
		t.Fatal(r)
	}
	id, _ := Hash([]byte("key"), "")
	if r := c.Chain().Get(ctx, id, Verify); r.Err != nil || r.Result.Value != "red" {
		t.Fatal(r)
	}
	locations := c.Locations()
	locations[0].Address = "changed"
	chain := c.Chain()
	chain[0].Store = nil
	if c.Locations()[0].Address != root || c.Chain()[0].Store == nil {
		t.Fatal("configuration changed")
	}
}

func TestClientDiscovery(t *testing.T) {
	srv := clientServer(t, &Memory{})
	t.Setenv("LOCK_PATH", srv.URL)
	c, err := Open(srv.URL)
	if err != nil || len(c.Locations()) != 1 {
		t.Fatal(c, err)
	}
	t.Setenv("LOCK_PATH", "")
	if len(c.Locations()) != 1 {
		t.Fatal("environment was not captured")
	}
	empty, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	requireErr(t, empty.Get(context.Background(), "k").Err, ErrNoSource)
	requireErr(t, empty.Create(context.Background(), "k", "v").Err, ErrNoWritableStore)
	for _, value := range []string{"relative", "ro=", "\"unclosed"} {
		t.Setenv("LOCK_PATH", value)
		if _, err := Open(srv.URL); err == nil {
			t.Fatal(value)
		}
	}
	c, err = OpenWithOptions(ClientOptions{Locations: []Location{}})
	if err != nil || len(c.Locations()) != 0 {
		t.Fatal(c, err)
	}
	if localSupported {
		lookup := func(name string) (string, bool) {
			if name == "HOME" || name == "XDG_CACHE_HOME" {
				return clientTempDir(t), true
			}
			return "", false
		}
		c, err = OpenWithOptions(ClientOptions{LookupEnv: lookup})
		if err != nil || len(c.Locations()) == 0 {
			t.Fatal(c, err)
		}
		c, err = OpenWithOptions(ClientOptions{Prepend: []Location{{Address: ".lock/locks"}}, Locations: []Location{}})
		want, _ := filepath.Abs(".lock/locks")
		if err != nil || c.Locations()[0].Address != want {
			t.Fatal(c, err)
		}
	}
}

func clientServer(t *testing.T, store Store) *httptest.Server {
	t.Helper()
	h, err := NewHandler(store, HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestClientPolicies(t *testing.T) {
	t.Setenv("LOCK_PATH", "")
	ctx := context.Background()
	a, b := &Memory{}, &Memory{}
	sa, sb := clientServer(t, a), clientServer(t, b)
	locs := []Location{{Address: sa.URL, CheckOnly: true, ReadOnly: true}, {Address: sb.URL}}
	c, err := OpenWithOptions(ClientOptions{Locations: locs, ReadPolicy: Verify, WritePolicy: WriteAll, Algorithm: "sha256", HTTP: HTTPOptions{Client: sb.Client(), POSTQueries: true}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := Hash([]byte("k"), "sha256")
	_, _ = a.Put(ctx, id, "witness", Create)
	if r := c.Create(ctx, "k", "source"); r.Err != nil || !r.Attempts[0].Skipped || r.Result.Status != Created {
		t.Fatal(r)
	}
	if r := c.Get(ctx, "k"); r.Err != nil || r.Result.Status != Conflict {
		t.Fatal(r)
	}
	c, err = OpenWithOptions(ClientOptions{Locations: locs, Algorithm: "sha256"})
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Get(ctx, "k"); r.Err != nil || r.Result.Value != "source" || !r.Attempts[0].Skipped {
		t.Fatal(r)
	}
	c, err = OpenWithOptions(ClientOptions{Prepend: []Location{{Address: sb.URL}}, Locations: locs})
	if err != nil || !reflect.DeepEqual(c.Locations(), []Location{locs[1], locs[0]}) {
		t.Fatal(c, err)
	}
	locs[0].Address = "mutated"
	if c.Locations()[1].Address != sa.URL {
		t.Fatal("caller mutated locations")
	}
	// A failed first write does not hide a later acknowledgement.
	denied := clientServer(t, failingStore{})
	c, err = Open(denied.URL, sb.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Create(ctx, "fallback", "v"); r.Err != nil || r.Attempts[0].Err == nil || r.Result.Status != Created {
		t.Fatal(r)
	}
}

func TestClientPerCallPolicies(t *testing.T) {
	ctx := context.Background()
	first, second := &Memory{}, &Memory{}
	a, b := clientServer(t, first), clientServer(t, second)
	c, err := OpenWithOptions(ClientOptions{
		Locations: []Location{{Address: a.URL}, {Address: b.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := Hash([]byte("shared"), "")
	_, _ = first.Put(ctx, id, "blue", Create)
	_, _ = second.Put(ctx, id, "red", Create)
	defaultRead := c.Get(ctx, "shared")
	if defaultRead.Err != nil || defaultRead.Result.Value != "blue" || defaultRead.Attempts[1].Attempted {
		t.Fatal(defaultRead)
	}
	verified := c.GetWithPolicy(ctx, "shared", Verify)
	if verified.Err != nil || verified.Result.Status != Conflict || !verified.Attempts[1].Attempted {
		t.Fatal(verified)
	}
	witness, err := OpenWithOptions(ClientOptions{
		Locations: []Location{{Address: a.URL, CheckOnly: true, ReadOnly: true}, {Address: b.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := witness.Get(ctx, "shared"); r.Err != nil || r.Result.Value != "red" || !r.Attempts[0].Skipped {
		t.Fatal(r)
	}
	if r := witness.GetWithPolicy(ctx, "shared", Verify); r.Err != nil || r.Result.Status != Conflict || !r.Attempts[0].Attempted {
		t.Fatal(r)
	}
	if again := c.Get(ctx, "shared"); again.Result.Value != "blue" || again.Attempts[1].Attempted {
		t.Fatal("per-call policy changed default", again)
	}
	if r := c.GetWithPolicy(ctx, "shared", ReadPolicy("invalid")); !errors.Is(r.Err, ErrFormat) {
		t.Fatal(r)
	}

	firstWrite := c.Create(ctx, "one", "value")
	if firstWrite.Err != nil || firstWrite.Result.Status != Created || firstWrite.Attempts[1].Attempted {
		t.Fatal(firstWrite)
	}
	one, _ := Hash([]byte("one"), "")
	if found, _ := second.Get(ctx, one); found.Status != Missing {
		t.Fatal(found)
	}
	allWrite := c.PutWithPolicy(ctx, "two", "value", Create, WriteAll)
	if allWrite.Err != nil || allWrite.Result.Status != Created || !allWrite.Attempts[0].Attempted || !allWrite.Attempts[1].Attempted {
		t.Fatal(allWrite)
	}
	two, _ := Hash([]byte("two"), "")
	if found, _ := second.Get(ctx, two); found.Status != Found || found.Value != "value" {
		t.Fatal(found)
	}
	if r := c.PutWithPolicy(ctx, "three", "value", Create, WritePolicy("invalid")); !errors.Is(r.Err, ErrFormat) {
		t.Fatal(r)
	}
	three, _ := Hash([]byte("three"), "")
	if found, _ := first.Get(ctx, three); found.Status != Missing {
		t.Fatal(found)
	}
	if r := c.Create(ctx, "four", "value"); r.Err != nil || r.Attempts[1].Attempted {
		t.Fatal("per-call policy changed default", r)
	}
	denied := clientServer(t, failingStore{})
	partial, err := OpenWithOptions(ClientOptions{
		Locations: []Location{{Address: a.URL}, {Address: denied.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := partial.PutWithPolicy(ctx, "partial", "value", Create, WriteAll); r.Err != nil || r.Result.Status != Created || !errors.Is(r.Attempts[1].Err, ErrDenied) {
		t.Fatal(r)
	}
}

type failingStore struct{}

func (failingStore) Get(context.Context, ID) (Result, error) { return Result{}, ErrDenied }
func (failingStore) Put(context.Context, ID, string, Mode) (Result, error) {
	return Result{}, ErrDenied
}

func TestClientInvalid(t *testing.T) {
	for _, options := range []ClientOptions{
		{Algorithm: "unknown"}, {RawKeyBytes: -1}, {ReadPolicy: "bad"}, {WritePolicy: "bad"},
		{Prepend: []Location{{}}}, {Locations: []Location{{Address: "relative"}}},
		{Prepend: []Location{{Address: "lo\x00cks"}}},
		{Prepend: []Location{{Address: "\x00/../locks"}}},
		{Locations: []Location{{Address: "http://"}}},
		{Locations: []Location{{Address: "https://example.com"}}, HTTP: HTTPOptions{Limits: Limits{ValueBytes: -1}}},
	} {
		if options.Locations == nil {
			options.Locations = []Location{}
		}
		if _, err := OpenWithOptions(options); err == nil {
			t.Fatal(options)
		}
	}
	if localSupported {
		_, err := OpenWithOptions(ClientOptions{Locations: []Location{{Address: filepath.Join(clientTempDir(t), "locks")}}, Local: LocalOptions{FileMode: os.ModeDir}})
		requireErr(t, err, ErrFormat)
	}
	c, err := OpenWithOptions(ClientOptions{Locations: []Location{}, RawKeyBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	requireErr(t, c.Get(context.Background(), "xx").Err, ErrLimit)
	requireErr(t, c.Create(context.Background(), "xx", "v").Err, ErrLimit)
}

func TestClientParentFailures(t *testing.T) {
	if !localSupported {
		t.Skip("local storage unsupported")
	}
	ctx := context.Background()
	for _, options := range []ClientOptions{
		{ExistingParents: true}, {Local: LocalOptions{ReadOnly: true}}, {},
	} {
		root := filepath.Join(clientTempDir(t), "missing", "locks")
		options.Locations = []Location{{Address: root}}
		c, err := OpenWithOptions(options)
		if err != nil {
			t.Fatal(err)
		}
		if options.ExistingParents || options.Local.ReadOnly {
			if r := c.Create(ctx, "k", "v"); r.Err == nil {
				t.Fatal(r)
			}
		} else {
			for _, value := range []string{"bad\nvalue", strings.Repeat("v", 1<<20+1)} {
				if r := c.Create(ctx, "k", value); r.Err == nil {
					t.Fatal(r)
				}
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			requireErr(t, c.Create(cancelled, "k", "v").Err, context.Canceled)
		}
		if _, err := os.Stat(filepath.Dir(root)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	root := filepath.Join(clientTempDir(t), "file")
	if err := os.WriteFile(root, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenWithOptions(ClientOptions{Locations: []Location{{Address: filepath.Join(root, "child", "locks")}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Create(ctx, "k", "v"); r.Err == nil {
		t.Fatal(r)
	}
}

func TestClientConcurrentParents(t *testing.T) {
	if !localSupported {
		t.Skip("local storage unsupported")
	}
	c, err := OpenWithOptions(ClientOptions{Locations: []Location{{Address: filepath.Join(clientTempDir(t), "a", "b", "locks")}}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := c.Create(context.Background(), "key", "value")
			if r.Err != nil || r.Result.Status != Created && r.Result.Status != Same {
				t.Error(r)
			}
		}()
	}
	wg.Wait()
}

func clientTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClientParentValidation(t *testing.T) {
	if !localSupported {
		t.Skip("local storage unsupported")
	}
	root := filepath.Join(clientTempDir(t), "missing", "locks")
	c, err := OpenWithOptions(ClientOptions{
		Locations: []Location{{Address: root}},
		Local:     LocalOptions{Limits: Limits{ValueBytes: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	requireErr(t, c.Create(context.Background(), "k", "too long").Err, ErrLimit)
	if _, err := os.Stat(filepath.Dir(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// A string key can contain bytes that are not UTF-8.
	key := "\xff\x00"
	if r := c.Create(context.Background(), key, "v"); r.Err != nil {
		t.Fatal(r)
	}
	id, _ := Hash([]byte(key), "")
	if r := c.Get(context.Background(), key); r.Err != nil || r.Result.ID != id || r.Result.Value != "v" {
		t.Fatal(r)
	}
}

func TestClientParentsRejectLinks(t *testing.T) {
	if !localSupported {
		t.Skip("local storage unsupported")
	}
	base, target := clientTempDir(t), clientTempDir(t)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	c, err := OpenWithOptions(ClientOptions{Locations: []Location{{Address: filepath.Join(link, "child", "locks")}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Create(context.Background(), "k", "v"); r.Err == nil {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(target, "child")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created directory through link", err)
	}
}
