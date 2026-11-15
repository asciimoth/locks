package locks

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testHandler(t *testing.T, store Store, opts HandlerOptions) *Handler {
	t.Helper()
	h, e := NewHandler(store, opts)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func serve(t *testing.T, h http.Handler, method, target, body, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable response")
	}
	return w
}
func TestHTTPRoundTrip(t *testing.T) {
	for _, post := range []bool{false, true} {
		t.Run(fmt.Sprint(post), func(t *testing.T) {
			m := &Memory{}
			h := testHandler(t, m, HandlerOptions{Limits: Limits{Entries: 16}})
			server := httptest.NewServer(h)
			defer server.Close()
			client, e := OpenHTTP(server.URL+"/base/?tenant=west#fragment", HTTPOptions{POSTQueries: post})
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			getWant(t, client, abc, Missing, "")
			putWant(t, client, abc, " x\t\ry \r\r", Create, Created)
			getWant(t, client, abc, Found, " x\t\ry ")
			putWant(t, client, abc, "other", Create, Conflict)
			putWant(t, client, abc, "", Replace, Replaced)
			putWant(t, client, abc, "\r", Create, Same)
			var writes []Write
			var ids []ID
			for i := 0; i < 40; i++ {
				id := testID(i)
				writes = append(writes, Write{id, fmt.Sprint(i), Create})
				ids = append(ids, id)
			}
			out, e := client.PutBatch(ctx, writes)
			if e != nil || len(out) != 40 {
				t.Fatal(out, e)
			}
			out, e = client.GetBatch(ctx, ids)
			if e != nil || len(out) != 40 {
				t.Fatal(out, e)
			}
			for i, o := range out {
				if o.Err != nil || o.Result.ID != ids[i] || o.Result.Value != fmt.Sprint(i) {
					t.Fatal(i, o)
				}
			}
		})
	}
}
func TestHTTPGETOnlySixteen(t *testing.T) {
	h := testHandler(t, &Memory{}, HandlerOptions{GETOnly: true})
	server := httptest.NewServer(h)
	defer server.Close()
	client, _ := OpenHTTP(server.URL, HTTPOptions{})
	ids := make([]ID, 16)
	for i := range ids {
		ids[i] = testID(i)
	}
	out, e := client.GetBatch(context.Background(), ids)
	if e != nil || len(out) != 16 {
		t.Fatal(out, e)
	}
	w := serve(t, h, "POST", "/lock", "locks/1 get\nraw.blake2.\n", "application/octet-stream")
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal(w)
	}
}
func TestHTTPRejectsBeforeWrites(t *testing.T) {
	requests := []struct {
		name, method, target, body, ctype string
		opts                              HandlerOptions
		status                            int
	}{
		{name: "duplicate aliases", method: "POST", target: "/lock", body: "locks/1 put\ncreate raw.blake2.YWJj first\nreplace id." + string(abc) + " second\n", ctype: "application/octet-stream", status: 400},
		{name: "unterminated", method: "POST", target: "/lock", body: "locks/1 put\ncreate raw.blake2.YWJj first\nreplace raw.blake2.YQ second", ctype: "application/octet-stream", status: 400},
		{name: "readonly", method: "POST", target: "/lock", body: "locks/1 put\ncreate raw.blake2.YWJj first\n", ctype: "application/octet-stream", opts: HandlerOptions{ReadOnly: true}, status: 403},
		{name: "authorization", method: "POST", target: "/lock", body: "locks/1 put\ncreate raw.blake2.YWJj first\ncreate raw.blake2.YQ second\n", ctype: "application/octet-stream", opts: HandlerOptions{Authorize: func(_ *http.Request, w Write) error {
			if w.ID != abc {
				return ErrDenied
			}
			return nil
		}}, status: 403},
		{name: "algorithm admission", method: "POST", target: "/lock", body: "locks/1 put\ncreate raw.blake2.YWJj first\ncreate raw.sha256.YQ second\n", ctype: "application/octet-stream", opts: HandlerOptions{AcceptAlgorithm: func(a string) bool { return a == "blake2" }}, status: 422},
		{name: "post query", method: "POST", target: "/lock?key=raw.blake2.", body: "locks/1 get\nraw.blake2.\n", ctype: "application/octet-stream", status: 400},
		{name: "media", method: "POST", target: "/lock", body: "locks/1 get\nraw.blake2.\n", ctype: "text/plain", status: 415},
		{name: "bad method", method: "DELETE", target: "/lock", status: 405},
		{name: "missing keys", method: "GET", target: "/lock?unknown=x", status: 400},
		{name: "get body", method: "GET", target: "/lock?key=raw.blake2.", body: "body", status: 400},
		{name: "bad query name", method: "GET", target: "/lock?%zz=x", status: 400},
		{name: "bad query value", method: "GET", target: "/lock?key=%zz", status: 400},
		{name: "bad token", method: "GET", target: "/lock?key=bad", status: 400},
		{name: "get duplicate", method: "GET", target: "/lock?key=raw.blake2.YWJj&key=id." + string(abc), status: 400},
		{name: "raw unsupported", method: "POST", target: "/lock", body: "locks/1 get\nraw.unknown.YQ\n", ctype: "application/octet-stream", status: 422},
		{name: "raw limit", method: "GET", target: "/lock?key=raw.blake2." + strings.Repeat("A", 400), opts: HandlerOptions{Limits: Limits{RawKeyBytes: 256}}, status: 413},
		{name: "target limit", method: "GET", target: "/lock?ignored=" + strings.Repeat("x", 5000), opts: HandlerOptions{Limits: Limits{TargetBytes: 4096}}, status: 414},
		{name: "body limit", method: "POST", target: "/lock", body: strings.Repeat("x", (128<<10)+1), ctype: "application/octet-stream", opts: HandlerOptions{Limits: Limits{BodyBytes: 128 << 10}}, status: 413},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Memory{}
			w := serve(t, testHandler(t, m, tt.opts), tt.method, tt.target, tt.body, tt.ctype)
			if w.Code != tt.status {
				t.Fatal(w.Code, w.Body.String())
			}
			getWant(t, m, abc, Missing, "")
		})
	}
	h := testHandler(t, &Memory{}, HandlerOptions{Limits: Limits{Entries: 16}})
	var query, body strings.Builder
	body.WriteString("locks/1 get\n")
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&query, "&key=id.%s", testID(i))
		fmt.Fprintf(&body, "id.%s\n", testID(i))
	}
	for _, w := range []*httptest.ResponseRecorder{serve(t, h, "GET", "/lock?"+query.String(), "", ""), serve(t, h, "POST", "/lock", body.String(), "application/octet-stream")} {
		if w.Code != 413 || w.Body.String() != "locks/1 error\ntoo-many 16\n" {
			t.Fatal(w)
		}
	}
	w := serve(t, h, "POST", "/lock", "locks/1 get\nraw.blake2.\n", "Application/Octet-Stream; charset=utf-8")
	if w.Code != 200 {
		t.Fatal(w)
	}
}
func TestHTTPHandlerBackendErrors(t *testing.T) {
	for _, code := range []error{ErrDenied, ErrLimit, ErrUnsupported, ErrIndeterminate, errors.New("other"), &RemoteError{"future"}, &RemoteError{"BAD"}} {
		h := testHandler(t, &stubStore{readErr: code}, HandlerOptions{})
		w := serve(t, h, "GET", "/lock?key=id."+string(abc), "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "error "+string(abc)) {
			t.Fatal(w)
		}
	}
	for _, r := range []Result{{Status: Found, Value: "\x00"}, {Status: Found, Value: strings.Repeat("x", 1025)}, {Status: Created}} {
		h := testHandler(t, &stubStore{read: r}, HandlerOptions{Limits: Limits{ValueBytes: 1024}})
		w := serve(t, h, "GET", "/lock?key=id."+string(abc), "", "")
		if r.Status == Created && w.Code != 500 {
			t.Fatal(w)
		}
	}
	for _, opts := range []HandlerOptions{{Limits: Limits{Entries: 1}}, {Limits: Limits{BodyBytes: -1}}} {
		_, e := NewHandler(&Memory{}, opts)
		requireErr(t, e, ErrLimit)
	}
	_, e := NewHandler(nil, HandlerOptions{})
	requireErr(t, e, ErrFormat)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeFail struct{ io.Reader }

func (closeFail) Close() error { return io.ErrClosedPipe }
func clientWith(t *testing.T, f transportFunc) *HTTPStore {
	t.Helper()
	c, e := OpenHTTP("http://example.test", HTTPOptions{Client: &http.Client{Transport: f}})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestHTTPClientInvalidResponses(t *testing.T) {
	ctx := context.Background()
	for _, write := range []bool{false, true} {
		for _, tt := range []struct {
			name, body, ctype string
			status            int
		}{{"missing media", "", "", 200}, {"bad version", "locks/2 results\n", "application/octet-stream", 200}, {"wrong id", "locks/1 results\nmissing " + string(testID(1)) + "\n", "application/octet-stream", 200}, {"extra", "locks/1 results\nmissing " + string(abc) + "\nmissing " + string(abc) + "\n", "application/octet-stream", 200}, {"truncated", "locks/1 results\nmissing " + string(abc), "application/octet-stream", 200}, {"endpoint missing", "", "text/html", 404}, {"server failure", "locks/1 error\nunavailable\n", "application/octet-stream", 503}, {"denied", "locks/1 error\ndenied\n", "application/octet-stream", 403}, {"limit", "", "text/html", 413}, {"redirect", "", "", 302}} {
			t.Run(fmt.Sprint(write, "/", tt.name), func(t *testing.T) {
				c := clientWith(t, func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tt.status, Header: http.Header{"Content-Type": {tt.ctype}}, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
				})
				var e error
				if write {
					_, e = c.Put(ctx, abc, "value", Replace)
				} else {
					_, e = c.Get(ctx, abc)
				}
				if e == nil {
					t.Fatal("accepted response")
				}
				if write && (tt.status == 200 || tt.status >= 500 || tt.status == 302) {
					requireErr(t, e, ErrIndeterminate)
				}
				var he *HTTPError
				if errors.As(e, &he) && he.Error() == "" {
					t.Fatal(e)
				}
			})
		}
	}
	for _, write := range []bool{false, true} {
		c := clientWith(t, func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })
		var e error
		if write {
			_, e = c.Put(ctx, abc, "", Create)
			requireErr(t, e, ErrIndeterminate)
		} else {
			_, e = c.Get(ctx, abc)
		}
		if e == nil {
			t.Fatal("transport error lost")
		}
	}
	c := clientWith(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: closeFail{strings.NewReader("locks/1 results\ncreated " + string(abc) + "\n")}}, nil
	})
	_, e := c.Put(ctx, abc, "", Create)
	requireErr(t, e, ErrIndeterminate)
	requireErr(t, e, io.ErrClosedPipe)
}
func TestHTTPClientValidationAndLimits(t *testing.T) {
	_, e := OpenHTTP("relative", HTTPOptions{})
	requireErr(t, e, ErrFormat)
	_, e = OpenHTTP("http://x", HTTPOptions{Limits: Limits{Entries: -1}})
	requireErr(t, e, ErrLimit)
	c := clientWith(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached network")
		return nil, nil
	})
	ctx := context.Background()
	for _, ids := range [][]ID{nil, {"bad"}, {abc, abc}} {
		_, e = c.GetBatch(ctx, ids)
		requireErr(t, e, ErrFormat)
	}
	_, e = c.PutBatch(ctx, []Write{{abc, "", Mode("bad")}})
	requireErr(t, e, ErrFormat)
	c.limits.TargetBytes = 1
	_, e = c.Get(ctx, abc)
	requireErr(t, e, ErrLimit)
	c.limits.TargetBytes = 1000
	c.limits.BodyBytes = 1
	_, e = c.Put(ctx, abc, "", Create)
	requireErr(t, e, ErrLimit)
	c.endpoint = "http://[bad"
	_, e = c.Get(ctx, abc)
	if e == nil {
		t.Fatal("bad URL accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	c.endpoint = "http://x"
	_, e = c.Get(cancelled, abc)
	if e == nil {
		t.Fatal("canceled request accepted")
	}
}
func TestHTTPBatchSplittingAndNoRedirects(t *testing.T) {
	h := testHandler(t, &Memory{}, HandlerOptions{})
	server := httptest.NewServer(h)
	defer server.Close()
	c, _ := OpenHTTP(server.URL, HTTPOptions{Limits: Limits{TargetBytes: 170, Entries: 2, BodyBytes: 180}})
	ids := []ID{testID(1), testID(2), testID(3)}
	ctx := context.Background()
	out, e := c.GetBatch(ctx, ids)
	if e != nil || len(out) != 3 {
		t.Fatal(out, e)
	}
	writes := []Write{{ids[0], "v", Create}, {ids[1], "v", Create}, {ids[2], "v", Create}}
	out, e = c.PutBatch(ctx, writes)
	if e != nil || len(out) != 3 {
		t.Fatal(out, e)
	}
	// A proxy may reject without a usable count. The client splits only a rejected batch.
	calls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		keys, _ := queryKeys(r.URL.RawQuery)
		if len(keys) > 1 {
			w.WriteHeader(414)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	c, _ = OpenHTTP(proxy.URL, HTTPOptions{})
	out, e = c.GetBatch(ctx, ids)
	if e != nil || len(out) != 3 || calls < 3 {
		t.Fatal(out, e, calls)
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	c, _ = OpenHTTP(redirect.URL, HTTPOptions{})
	_, e = c.Put(ctx, abc, "secret", Replace)
	requireErr(t, e, ErrIndeterminate)
	if targetCalls != 0 {
		t.Fatal("followed redirect")
	}
}

func TestMinimumPOSTCapacity(t *testing.T) {
	h := testHandler(t, &Memory{}, HandlerOptions{Limits: Limits{RawKeyBytes: 256, ValueBytes: 1024, LineBytes: 2048, Entries: 16, BodyBytes: 128 << 10, TargetBytes: 4096}})
	var b strings.Builder
	b.WriteString("locks/1 put\n")
	for i := 0; i < 16; i++ {
		raw := append([]byte{byte(i)}, []byte(strings.Repeat("x", 255))...)
		fmt.Fprintf(&b, "create raw.blake2.%s %s\n", base64.RawURLEncoding.EncodeToString(raw), strings.Repeat("v", 1024))
	}
	w := serve(t, h, "POST", "/lock", b.String(), "application/octet-stream")
	if w.Code != 200 || strings.Count(w.Body.String(), "created ") != 16 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSplitWriteFailureIsIndeterminate(t *testing.T) {
	ctx := context.Background()
	items := []Write{{testID(1), "v", Create}, {testID(2), "v", Create}}
	for _, serverSplit := range []bool{false, true} {
		t.Run(fmt.Sprint(serverSplit), func(t *testing.T) {
			calls := 0
			c := clientWith(t, func(r *http.Request) (*http.Response, error) {
				calls++
				req, e := DecodeRequest(r.Body, Limits{})
				if e != nil {
					t.Fatal(e)
				}
				status, body := 200, ""
				if len(req.Items) > 1 {
					status = 413
					body = "locks/1 error\nlimit\n"
				} else if req.Items[0].ID == items[0].ID {
					body = "locks/1 results\ncreated " + string(items[0].ID) + "\n"
				} else {
					status = 403
					body = "locks/1 error\ndenied\n"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if !serverSplit {
				c.limits.Entries = 1
			}
			_, e := c.PutBatch(ctx, items)
			requireErr(t, e, ErrIndeterminate)
			requireErr(t, e, ErrDenied)
			if calls < 2 {
				t.Fatal(calls)
			}
		})
	}
}
