package locks

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type failingResponseWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w failingResponseWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }
func (w failingResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestHandlerWriteErrors(t *testing.T) {
	for _, target := range []string{"/lock", "/lock?key=raw.blake2.YWJj"} {
		for _, writeErr := range []error{nil, io.ErrClosedPipe} {
			var output bytes.Buffer
			server := &http.Server{ErrorLog: log.New(&output, "", 0)}
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, server))
			w := failingResponseWriter{httptest.NewRecorder(), writeErr}
			testHandler(t, &Memory{}, HandlerOptions{}).ServeHTTP(w, r)
			want := io.ErrShortWrite
			if writeErr != nil {
				want = writeErr
			}
			if !strings.Contains(output.String(), want.Error()) {
				t.Fatalf("write error not logged: %q", output.String())
			}
		}
	}
	// Direct handler calls do not have a server in the request context.
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	writeResponse(failingResponseWriter{httptest.NewRecorder(), io.ErrClosedPipe}, httptest.NewRequest("GET", "/", nil), "body")
	if !strings.Contains(output.String(), io.ErrClosedPipe.Error()) {
		t.Fatal("write error not logged without a server")
	}
}

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
	wrong := testID(1)
	for _, tt := range []struct {
		method, target, body, contentType string
		store                             *stubStore
	}{
		{"GET", "/lock?key=id." + string(abc), "", "", &stubStore{read: Result{ID: wrong, Status: Found, Value: "wrong"}}},
		{"POST", "/lock", "locks/1 put\ncreate id." + string(abc) + " value\n", "application/octet-stream", &stubStore{write: Result{ID: wrong, Status: Created}}},
	} {
		w := serve(t, testHandler(t, tt.store, HandlerOptions{}), tt.method, tt.target, tt.body, tt.contentType)
		if w.Code != 500 || w.Body.String() != "locks/1 error\nunavailable\n" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, opts := range []HandlerOptions{{Limits: Limits{Entries: 1}}, {Limits: Limits{BodyBytes: -1}}} {
		_, e := NewHandler(&Memory{}, opts)
		requireErr(t, e, ErrLimit)
	}
	_, e := NewHandler(nil, HandlerOptions{})
	requireErr(t, e, ErrFormat)
}

type batchStubStore struct {
	*stubStore
	outcomes []Outcome
}

func (s *batchStubStore) GetBatch(context.Context, []ID) ([]Outcome, error) {
	return s.outcomes, nil
}

func (s *batchStubStore) PutBatch(context.Context, []Write) ([]Outcome, error) {
	return s.outcomes, nil
}

func TestHTTPHandlerBatchErrorIDs(t *testing.T) {
	for _, id := range []ID{"", testID(1)} {
		store := &batchStubStore{stubStore: &stubStore{}, outcomes: []Outcome{{Result: Result{ID: id}, Err: ErrDenied}}}
		for _, tt := range []struct{ method, target, body, contentType string }{
			{"GET", "/lock?key=id." + string(abc), "", ""},
			{"POST", "/lock", "locks/1 put\ncreate id." + string(abc) + " value\n", "application/octet-stream"},
		} {
			w := serve(t, testHandler(t, store, HandlerOptions{}), tt.method, tt.target, tt.body, tt.contentType)
			want := "locks/1 results\nerror " + string(abc) + " denied\n"
			if w.Code != 200 || w.Body.String() != want {
				t.Fatalf("result ID %q: HTTP %d %q", id, w.Code, w.Body.String())
			}
		}
	}
}

func TestHTTPHandlerInvalidBatchResults(t *testing.T) {
	for _, put := range []bool{false, true} {
		status := Missing
		method, target, body, contentType := "GET", "/lock?key=id."+string(abc), "", ""
		if put {
			status = Created
			method, target, body, contentType = "POST", "/lock", "locks/1 put\ncreate id."+string(abc)+" value\n", "application/octet-stream"
		}
		valid := Outcome{Result: Result{ID: abc, Status: status}}
		for _, outcomes := range [][]Outcome{nil, {valid, valid}, {{Result: Result{ID: testID(1), Status: status}}}, {{Result: Result{Status: status}}}} {
			store := &batchStubStore{stubStore: &stubStore{}, outcomes: outcomes}
			w := serve(t, testHandler(t, store, HandlerOptions{}), method, target, body, contentType)
			if w.Code != 500 || w.Body.String() != "locks/1 error\nunavailable\n" {
				t.Fatalf("put %v, outcomes %v: HTTP %d %q", put, outcomes, w.Code, w.Body.String())
			}
		}
	}
}

func TestHTTPHandlerRejectsMalformedBackendFields(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, tt := range []struct {
			name   string
			put    bool
			result Result
		}{
			{"read-id-space", false, Result{ID: ID(string(abc) + " injected"), Status: Found, Value: "value"}},
			{"read-status-space", false, Result{ID: abc, Status: Status("found " + string(abc))}},
			{"read-id-cr", false, Result{ID: ID(string(abc) + "\r"), Status: Missing}},
			{"write-id-cr", true, Result{ID: ID(string(abc) + "\r"), Status: Created}},
			{"read-missing-value", false, Result{ID: abc, Status: Missing, Value: "stray"}},
			{"write-created-value", true, Result{ID: abc, Status: Created, Value: "stray"}},
		} {
			t.Run(fmt.Sprintf("%s/batch=%v", tt.name, batch), func(t *testing.T) {
				stub := &stubStore{read: tt.result, write: tt.result}
				var store Store = stub
				if batch {
					store = &batchStubStore{stubStore: stub, outcomes: []Outcome{{Result: tt.result}}}
				}
				method, target, body, contentType := "GET", "/lock?key=id."+string(abc), "", ""
				if tt.put {
					method, target, body, contentType = "POST", "/lock", "locks/1 put\ncreate id."+string(abc)+" value\n", "application/octet-stream"
				}
				w := serve(t, testHandler(t, store, HandlerOptions{}), method, target, body, contentType)
				if w.Code != 500 || w.Body.String() != "locks/1 error\nunavailable\n" {
					t.Fatalf("HTTP %d %q", w.Code, w.Body.String())
				}
			})
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func TestHTTPHandlerPreservesIndeterminate(t *testing.T) {
	for _, code := range []string{"denied", "limit", "future"} {
		t.Run(code, func(t *testing.T) {
			store := &stubStore{writeErr: errors.Join(ErrIndeterminate, &RemoteError{Code: code})}
			server := httptest.NewServer(testHandler(t, store, HandlerOptions{}))
			defer server.Close()
			client, err := OpenHTTP(server.URL, HTTPOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Put(context.Background(), abc, "new", Replace)
			requireErr(t, err, ErrIndeterminate)
		})
	}
}

func TestHTTPHandlerInvalidWriteStatusIsIndeterminate(t *testing.T) {
	for _, value := range []string{"value", "bad\nvalue", strings.Repeat("x", 1025)} {
		store := &stubStore{write: Result{Status: Found, Value: value}}
		server := httptest.NewServer(testHandler(t, store, HandlerOptions{Limits: Limits{ValueBytes: 1024}}))
		client, err := OpenHTTP(server.URL, HTTPOptions{})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		_, err = client.Put(context.Background(), abc, "new", Replace)
		server.Close()
		requireErr(t, err, ErrIndeterminate)
	}
}

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

func TestHTTPRejectsOversizedItemsBeforeSending(t *testing.T) {
	for _, budget := range []string{"line", "body"} {
		t.Run(budget, func(t *testing.T) {
			store := &Memory{}
			calls := 0
			h := testHandler(t, store, HandlerOptions{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				h.ServeHTTP(w, r)
			}))
			defer server.Close()
			limits := Limits{Entries: 1}
			if budget == "line" {
				limits.LineBytes = 100
			} else {
				limits.BodyBytes = 100
			}
			client, err := OpenHTTP(server.URL, HTTPOptions{Limits: limits})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.PutBatch(context.Background(), []Write{
				{abc, "fits", Create},
				{testID(1), strings.Repeat("v", 100), Replace},
			})
			requireErr(t, err, ErrLimit)
			if errors.Is(err, ErrIndeterminate) || calls != 0 {
				t.Fatalf("invalid batch reached server: calls=%d, err=%v", calls, err)
			}
			getWant(t, store, abc, Missing, "")
		})
	}
}

func TestHTTPResponseBodyBudget(t *testing.T) {
	store := &Memory{}
	ids := []ID{abc, testID(1)}
	for _, id := range ids {
		putWant(t, store, id, strings.Repeat("v", 64<<10), Create, Created)
	}
	h := testHandler(t, store, HandlerOptions{Limits: Limits{BodyBytes: 128 << 10}})
	body := encodeRequest(Request{Items: []Write{{ID: ids[0]}, {ID: ids[1]}}})
	w := serve(t, h, "POST", "/lock", body, "application/octet-stream")
	if w.Code != 500 || w.Body.String() != "locks/1 error\nunavailable\n" {
		t.Fatalf("oversized response: %d, %d bytes", w.Code, w.Body.Len())
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

func TestHTTPUnconfirmedWriteIsIndeterminate(t *testing.T) {
	for _, status := range []int{101, 201, 202, 204, 206, 408, 409, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := &Memory{}
			calls := 0
			client := clientWith(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if _, err := store.Put(r.Context(), abc, "new", Replace); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			_, err := client.Put(context.Background(), abc, "new", Replace)
			requireErr(t, err, ErrIndeterminate)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status || calls != 1 {
				t.Fatal(err, calls)
			}
			getWant(t, store, abc, Found, "new")
		})
	}
}

func TestHTTPRejectedWriteIsDefinite(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 405, 413, 414, 415, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := clientWith(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			_, err := client.Put(context.Background(), abc, "new", Replace)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status || errors.Is(err, ErrIndeterminate) {
				t.Fatalf("rejected write: %v", err)
			}
		})
	}
}

func TestHTTPIndeterminateLimitIsNotRetried(t *testing.T) {
	calls := 0
	client := clientWith(t, func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 413, Body: closeFail{strings.NewReader("")}}, nil
	})
	_, err := client.PutBatch(context.Background(), []Write{{abc, "new", Replace}, {testID(1), "new", Replace}})
	requireErr(t, err, ErrIndeterminate)
	requireErr(t, err, io.ErrClosedPipe)
	if calls != 1 {
		t.Fatalf("sent %d requests after an indeterminate response", calls)
	}
}

func TestHTTPIndeterminateErrorBodyIsNotRetried(t *testing.T) {
	for _, status := range []int{413, 414} {
		for _, count := range []int{1, 2} {
			t.Run(fmt.Sprintf("%d/%d", status, count), func(t *testing.T) {
				calls := 0
				client := clientWith(t, func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{
						StatusCode: status,
						Header:     http.Header{"Content-Type": {"application/octet-stream"}},
						Body:       io.NopCloser(strings.NewReader("locks/1 error\nindeterminate\n")),
					}, nil
				})
				writes := []Write{{abc, "new", Replace}, {testID(1), "new", Replace}}
				_, err := client.PutBatch(context.Background(), writes[:count])
				requireErr(t, err, ErrIndeterminate)
				requireErr(t, err, ErrLimit)
				if calls != 1 {
					t.Fatalf("sent %d requests after an indeterminate response", calls)
				}
			})
		}
	}
}

func TestHTTPDecreasingAdvertisedLimits(t *testing.T) {
	calls := 0
	client := clientWith(t, func(r *http.Request) (*http.Response, error) {
		calls++
		req, err := DecodeRequest(r.Body, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		status, body := http.StatusOK, ""
		if len(req.Items) > 16 {
			status = http.StatusRequestEntityTooLarge
			body = errorBody("too-many", len(req.Items)-1)
		} else {
			out := make([]Outcome, len(req.Items))
			for i, item := range req.Items {
				out[i].Result = Result{ID: item.ID, Status: Missing}
			}
			body, err = encodeResults(out, Limits{})
			if err != nil {
				t.Fatal(err)
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	client.postQueries = true
	ids := make([]ID, 128)
	for i := range ids {
		ids[i] = testID(i)
	}
	out, err := client.GetBatch(context.Background(), ids)
	if err != nil || len(out) != len(ids) {
		t.Fatalf("decreasing limits: %d results, %v", len(out), err)
	}
	for i, result := range out {
		if result.Err != nil || result.Result.ID != ids[i] || result.Result.Status != Missing {
			t.Fatalf("result %d: %+v", i, result)
		}
	}
	if calls > 15 {
		t.Fatalf("decreasing peer limits caused %d calls, want at most 15", calls)
	}
}

func TestHandlerNilRequest(t *testing.T) {
	h := testHandler(t, &Memory{}, HandlerOptions{})
	for _, r := range []*http.Request{nil, {}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("nil request or URL: %d", w.Code)
		}
	}
	writeResponse(failingResponseWriter{httptest.NewRecorder(), io.ErrClosedPipe}, nil, "body")
}

func TestHandlerRejectsUnknownLengthGET(t *testing.T) {
	store := &Memory{}
	h := testHandler(t, store, HandlerOptions{})
	r := (&http.Request{Method: http.MethodGet, URL: &url.URL{RawQuery: "key=raw.blake2.YWJj"}, Body: io.NopCloser(strings.NewReader("body")), ContentLength: -1}).WithContext(context.Background())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown-length GET: %d", w.Code)
	}
	var remote *RemoteError
	if err := DecodeError(strings.NewReader(w.Body.String()), w.Code, Limits{}); !errors.As(err, &remote) || remote.Code != "malformed" {
		t.Fatalf("malformed GET rejection: %v", err)
	}
}

func TestNilBackendErrors(t *testing.T) {
	if errors.Is((*TooManyError)(nil), ErrLimit) {
		t.Fatal("nil count error has a limit cause")
	}
	for _, err := range []error{(*RemoteError)(nil), (*TooManyError)(nil), (*HTTPError)(nil)} {
		// Exercise direct calls as well as wrapped errors from a backend.
		if err.Error() == "" {
			t.Fatal("empty diagnostic")
		}
		for _, wrapped := range []error{err, fmt.Errorf("backend: %w", err), errors.Join(err, io.ErrUnexpectedEOF)} {
			h := testHandler(t, &stubStore{readErr: wrapped}, HandlerOptions{})
			r := httptest.NewRequest(http.MethodGet, "/lock?key=raw.blake2.YWJj", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("backend error response: %d", w.Code)
			}
			out, decodeErr := DecodeResults(strings.NewReader(w.Body.String()), Request{Items: []Write{{ID: abc}}}, Limits{})
			if decodeErr != nil || len(out) != 1 || out[0].Err == nil {
				t.Fatalf("lost backend error: %+v %v", out, decodeErr)
			}
		}
	}
}

func TestHTTPNilCountReadError(t *testing.T) {
	calls := 0
	client := clientWith(t, func(*http.Request) (*http.Response, error) {
		calls++
		body := readerFunc(func([]byte) (int, error) { return 0, (*TooManyError)(nil) })
		return &http.Response{StatusCode: 413, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(body)}, nil
	})
	_, err := client.GetBatch(context.Background(), []ID{abc, testID(1)})
	if err == nil || calls != 1 {
		t.Fatalf("invalid count error: %v, %d calls", err, calls)
	}
}
