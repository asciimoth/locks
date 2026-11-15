package locks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func FuzzHandler(f *testing.F) {
	f.Add("GET", "key=raw.blake2.YWJj", "", "")
	f.Add("GET", "key=%zz", "", "")
	f.Add("POST", "", "locks/1 put\ncreate raw.blake2.YWJj value\n", "application/octet-stream")
	f.Add("POST", "", "locks/1 get\nraw.blake2.YWJj\n", "application/octet-stream")
	f.Add("DELETE", "", "", "")
	f.Fuzz(func(t *testing.T, method, query, body, mediaType string) {
		h := testHandler(t, &Memory{}, HandlerOptions{})
		r := &http.Request{
			Method: method, URL: &url.URL{Path: "/lock", RawQuery: query},
			RequestURI: "/lock?" + query,
			Header:     http.Header{"Content-Type": {mediaType}},
			Body:       io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" || !mediaOK(w.Header().Get("Content-Type")) {
			t.Fatal("invalid response headers")
		}
		if w.Code != http.StatusOK {
			if err := DecodeError(strings.NewReader(w.Body.String()), w.Code, Limits{}); err == ErrProtocol {
				t.Fatal("invalid rejection body", w.Body.String())
			}
			return
		}
		var req Request
		if method == http.MethodPost {
			var err error
			req, err = DecodeRequest(strings.NewReader(body), Limits{})
			if err != nil {
				t.Fatal("accepted malformed request", err)
			}
		} else {
			keys, err := queryKeys(query)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				id, err := ParseKey(key, Limits{})
				if err != nil {
					t.Fatal(err)
				}
				req.Items = append(req.Items, Write{ID: id})
			}
		}
		if _, err := DecodeResults(strings.NewReader(w.Body.String()), req, Limits{}); err != nil {
			t.Fatal("invalid success body", err)
		}
	})
}
