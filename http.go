package locks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func errorCode(e error) string {
	if errors.Is(e, ErrIndeterminate) {
		return "indeterminate"
	}
	var remote *RemoteError
	if errors.As(e, &remote) && validName(remote.Code) && remote.Code != "too-many" {
		return remote.Code
	}
	switch {
	case errors.Is(e, ErrDenied):
		return "denied"
	case errors.Is(e, ErrLimit):
		return "limit"
	case errors.Is(e, ErrUnsupported):
		return "unsupported"
	default:
		return "unavailable"
	}
}
func mediaOK(s string) bool {
	typ, _, e := mime.ParseMediaType(s)
	return e == nil && strings.EqualFold(typ, "application/octet-stream")
}

// HandlerOptions configures the protocol surface. Authorize runs for every item
// before any write. It must authorize the requested read/create/replace operation.
type HandlerOptions struct {
	ReadOnly, GETOnly bool
	Limits            Limits
	Authorize         func(*http.Request, Write) error
	AcceptAlgorithm   func(string) bool
}

// Handler implements the endpoint at whichever route the caller mounts it on.
// It makes no durability promise beyond that of Store.
type Handler struct {
	store   Store
	options HandlerOptions
	limits  Limits
}

func NewHandler(store Store, options HandlerOptions) (*Handler, error) {
	if store == nil {
		return nil, ErrFormat
	}
	l, e := options.Limits.normalized()
	if e != nil {
		return nil, e
	}
	if l.Entries < 16 || l.TargetBytes < 4096 || l.BodyBytes < 128<<10 || l.RawKeyBytes < 256 || l.ValueBytes < 1024 || l.LineBytes < 2048 {
		return nil, ErrLimit
	}
	return &Handler{store, options, l}, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	fail := func(status int, code string, max int) {
		w.WriteHeader(status)
		writeResponse(w, r, errorBody(code, max))
	}
	if len(r.RequestURI) > h.limits.TargetBytes {
		fail(414, "limit", 0)
		return
	}
	keys, e := queryKeys(r.URL.RawQuery)
	if e != nil {
		fail(400, "malformed", 0)
		return
	}
	req := Request{}
	switch r.Method {
	case http.MethodGet:
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(400, "malformed", 0)
			return
		}
		if len(keys) == 0 {
			fail(400, "malformed", 0)
			return
		}
		if len(keys) > h.limits.Entries {
			fail(413, "too-many", h.limits.Entries)
			return
		}
		seen := map[ID]bool{}
		for _, key := range keys {
			id, e := ParseKey(key, h.limits)
			if e != nil {
				h.reject(fail, e)
				return
			}
			if seen[id] {
				fail(400, "duplicate", 0)
				return
			}
			seen[id] = true
			req.Items = append(req.Items, Write{ID: id})
		}
	case http.MethodPost:
		if h.options.GETOnly {
			w.Header().Set("Allow", "GET")
			fail(405, "unsupported", 0)
			return
		}
		if len(keys) > 0 {
			fail(400, "malformed", 0)
			return
		}
		if !mediaOK(r.Header.Get("Content-Type")) {
			fail(415, "unsupported", 0)
			return
		}
		req, e = DecodeRequest(r.Body, h.limits)
		if e != nil {
			h.reject(fail, e)
			return
		}
		if req.Put && h.options.ReadOnly {
			fail(403, "denied", 0)
			return
		}
	default:
		allow := "GET, POST"
		if h.options.GETOnly {
			allow = "GET"
		}
		w.Header().Set("Allow", allow)
		fail(405, "unsupported", 0)
		return
	}
	for _, item := range req.Items {
		_, alg, _ := strings.Cut(string(item.ID), ":")
		if h.options.AcceptAlgorithm != nil && !h.options.AcceptAlgorithm(alg) {
			fail(422, "unsupported", 0)
			return
		}
		if h.options.Authorize != nil {
			if e = h.options.Authorize(r, item); e != nil {
				fail(403, "denied", 0)
				return
			}
		}
	}
	var outcomes []Outcome
	if batch, ok := h.store.(BatchStore); ok {
		if req.Put {
			outcomes, e = batch.PutBatch(r.Context(), req.Items)
		} else {
			ids := make([]ID, len(req.Items))
			for i, item := range req.Items {
				ids[i] = item.ID
			}
			outcomes, e = batch.GetBatch(r.Context(), ids)
		}
		if e != nil {
			outcomes = make([]Outcome, len(req.Items))
			for i, item := range req.Items {
				outcomes[i] = Outcome{Result: Result{ID: item.ID}, Err: e}
			}
		}
	} else {
		outcomes = make([]Outcome, len(req.Items))
		for i, item := range req.Items {
			var result Result
			var e error
			if req.Put {
				result, e = h.store.Put(r.Context(), item.ID, item.Value, item.Mode)
			} else {
				result, e = h.store.Get(r.Context(), item.ID)
			}
			outcomes[i] = Outcome{result, e}
		}
	}
	if len(outcomes) != len(req.Items) {
		fail(500, "unavailable", 0)
		return
	}
	for i := range outcomes {
		if outcomes[i].Err != nil {
			outcomes[i].Result.ID = req.Items[i].ID
			continue
		}
		result := outcomes[i].Result
		valid := result.Status == Found || result.Status == Missing
		if req.Put {
			valid = result.Status == Created || result.Status == Same ||
				result.Status == Replaced && req.Items[i].Mode == Replace ||
				result.Status == Conflict && req.Items[i].Mode == Create
		}
		// Check fields before encoding: separators or trailing CR bytes must
		// not turn a malformed backend result into a valid wire record.
		if result.ID != req.Items[i].ID || !valid {
			fail(500, "unavailable", 0)
			return
		}
		if result.Status == Found {
			outcomes[i].Result.Value, outcomes[i].Err = NormalizeValue(outcomes[i].Result.Value)
			if len(outcomes[i].Result.Value) > h.limits.ValueBytes {
				outcomes[i].Err = ErrLimit
			}
		}
	}
	body, e := encodeResults(outcomes, h.limits)
	if e != nil {
		fail(500, "unavailable", 0)
		return
	}
	if _, e = DecodeResults(strings.NewReader(body), req, h.limits); e != nil {
		fail(500, "unavailable", 0)
		return
	}
	writeResponse(w, r, body)
}

// Headers can already be sent when a write fails. Report the failure through
// the server logger; sending a second response cannot repair a partial body.
func writeResponse(w http.ResponseWriter, r *http.Request, body string) {
	n, err := io.WriteString(w, body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		logger := log.Default()
		if server, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok && server != nil && server.ErrorLog != nil {
			logger = server.ErrorLog
		}
		logger.Printf("locks: response write: %v", err)
	}
}
func (h *Handler) reject(fail func(int, string, int), e error) {
	var count *TooManyError
	switch {
	case errors.As(e, &count):
		fail(413, "too-many", h.limits.Entries)
	case errors.Is(e, ErrLimit):
		fail(413, "limit", 0)
	case errors.Is(e, ErrUnsupported):
		fail(422, "unsupported", 0)
	default:
		fail(400, "malformed", 0)
	}
}

// HTTPOptions configures outbound limits and the transport. Redirects are disabled.
// Queries use GET for GET-only compatibility unless POSTQueries is selected.
type HTTPOptions struct {
	Client      *http.Client
	Limits      Limits
	POSTQueries bool
}
type HTTPStore struct {
	endpoint    string
	client      *http.Client
	limits      Limits
	postQueries bool
}

func OpenHTTP(base string, options HTTPOptions) (*HTTPStore, error) {
	endpoint, e := Endpoint(base)
	if e != nil {
		return nil, e
	}
	l, e := options.Limits.normalized()
	if e != nil {
		return nil, e
	}
	client := http.Client{Timeout: 30 * time.Second}
	if options.Client != nil {
		client = *options.Client
		if client.Timeout == 0 {
			client.Timeout = 30 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPStore{endpoint, &client, l, options.POSTQueries}, nil
}
func (s *HTTPStore) Get(ctx context.Context, id ID) (Result, error) {
	out, e := s.GetBatch(ctx, []ID{id})
	if e != nil {
		return Result{}, e
	}
	return out[0].Result, out[0].Err
}
func (s *HTTPStore) Put(ctx context.Context, id ID, value string, mode Mode) (Result, error) {
	out, e := s.PutBatch(ctx, []Write{{id, value, mode}})
	if e != nil {
		return Result{}, e
	}
	return out[0].Result, out[0].Err
}
func (s *HTTPStore) GetBatch(ctx context.Context, ids []ID) ([]Outcome, error) {
	items := make([]Write, len(ids))
	for i, id := range ids {
		items[i].ID = id
	}
	return s.batch(ctx, Request{Items: items})
}
func (s *HTTPStore) PutBatch(ctx context.Context, items []Write) ([]Outcome, error) {
	return s.batch(ctx, Request{Put: true, Items: append([]Write(nil), items...)})
}
func (s *HTTPStore) batch(ctx context.Context, req Request) ([]Outcome, error) {
	if len(req.Items) == 0 {
		return nil, ErrFormat
	}
	seen := map[ID]bool{}
	for i, w := range req.Items {
		if _, e := ParseID(string(w.ID)); e != nil {
			return nil, e
		}
		if seen[w.ID] {
			return nil, ErrFormat
		}
		seen[w.ID] = true
		if req.Put {
			v, e := validateWrite(w, s.limits)
			if e != nil {
				return nil, e
			}
			req.Items[i] = v
		}
		if req.Put || s.postQueries {
			// Check every item before sending the first sub-batch. A single
			// oversized record cannot be made smaller by splitting the batch.
			n := requestLineSize(req.Items[i], req.Put)
			if len("locks/1 put") > s.limits.LineBytes || n > s.limits.LineBytes ||
				n+1+len("locks/1 put\n") > s.limits.BodyBytes {
				return nil, ErrLimit
			}
		}
	}
	var result []Outcome
	for start := 0; start < len(req.Items); {
		end := start + s.limits.Entries
		if end > len(req.Items) {
			end = len(req.Items)
		}
		out, e := s.sendSplit(ctx, Request{req.Put, req.Items[start:end]})
		if e != nil {
			if req.Put && len(result) > 0 {
				e = errors.Join(ErrIndeterminate, e)
			}
			return nil, e
		}
		result = append(result, out...)
		start = end
	}
	return result, nil
}
func (s *HTTPStore) sendSplit(ctx context.Context, req Request) ([]Outcome, error) {
	out, e := s.send(ctx, req)
	var many *TooManyError
	if errors.As(e, &many) && !errors.Is(e, ErrIndeterminate) && len(req.Items) > 1 {
		n := many.Max
		if n <= 0 || n >= len(req.Items) {
			n = len(req.Items) / 2
		}
		var all []Outcome
		for i := 0; i < len(req.Items); i += n {
			end := i + n
			if end > len(req.Items) {
				end = len(req.Items)
			}
			part, err := s.sendSplit(ctx, Request{req.Put, req.Items[i:end]})
			if err != nil {
				if req.Put && len(all) > 0 {
					err = errors.Join(ErrIndeterminate, err)
				}
				return nil, err
			}
			all = append(all, part...)
		}
		return all, nil
	}
	return out, e
}

// HTTPError preserves the HTTP status even when an intermediary sends a non-v1 body.
type HTTPError struct {
	StatusCode int
	Cause      error
}

func (e *HTTPError) Error() string { return fmt.Sprintf("locks: HTTP %d: %v", e.StatusCode, e.Cause) }
func (e *HTTPError) Unwrap() error { return e.Cause }
func (s *HTTPStore) send(ctx context.Context, req Request) (out []Outcome, err error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	method, address, body := http.MethodGet, s.endpoint, ""
	if req.Put || s.postQueries {
		method = http.MethodPost
		size := len("locks/1 put\n")
		for _, item := range req.Items {
			n := requestLineSize(item, req.Put) + 1
			if n > s.limits.BodyBytes-size {
				if len(req.Items) > 1 {
					return nil, &TooManyError{len(req.Items) / 2}
				}
				return nil, ErrLimit
			}
			size += n
		}
		body = encodeRequest(req)
	} else {
		sep := "?"
		if strings.Contains(address, "?") {
			sep = "&"
		}
		for _, w := range req.Items {
			address += sep + "key=" + url.QueryEscape("id."+string(w.ID))
			sep = "&"
		}
	}
	u, e := url.Parse(address)
	if e != nil {
		return nil, e
	}
	if len(u.RequestURI()) > s.limits.TargetBytes {
		if len(req.Items) > 1 {
			return nil, &TooManyError{len(req.Items) / 2}
		}
		return nil, ErrLimit
	}
	request, e := http.NewRequestWithContext(ctx, method, address, strings.NewReader(body))
	if e != nil {
		return nil, e
	}
	request.Header.Set("Cache-Control", "no-store")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	response, e := s.client.Do(request)
	if e != nil {
		if req.Put {
			return nil, errors.Join(ErrIndeterminate, e)
		}
		return nil, e
	}
	defer func() {
		if e := response.Body.Close(); e != nil {
			out = nil
			err = errors.Join(err, e)
			if req.Put {
				err = errors.Join(ErrIndeterminate, err)
			}
		}
	}()
	if response.StatusCode != 200 {
		cause := error(ErrProtocol)
		if mediaOK(response.Header.Get("Content-Type")) {
			cause = DecodeError(response.Body, response.StatusCode, s.limits)
		}
		if response.StatusCode == 413 || response.StatusCode == 414 {
			var many *TooManyError
			if !errors.As(cause, &many) {
				if len(req.Items) > 1 {
					cause = errors.Join(cause, &TooManyError{len(req.Items) / 2})
				} else {
					cause = errors.Join(cause, ErrLimit)
				}
			}
		}
		if response.StatusCode == 401 || response.StatusCode == 403 {
			cause = errors.Join(ErrDenied, cause)
		}
		err = &HTTPError{response.StatusCode, cause}
		if req.Put {
			switch response.StatusCode {
			case 400, 401, 403, 404, 405, 413, 414, 415, 422:
				// These statuses reject the request before execution.
			default:
				err = errors.Join(ErrIndeterminate, err)
			}
		}
		return nil, err
	}
	if !mediaOK(response.Header.Get("Content-Type")) {
		err = ErrProtocol
	} else {
		out, err = DecodeResults(response.Body, req, s.limits)
	}
	if err != nil && req.Put {
		err = errors.Join(ErrIndeterminate, err)
	}
	return out, err
}
