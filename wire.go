package locks

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Request is a validated wire request. Each Write has an empty Mode for queries.
type Request struct {
	Put   bool
	Items []Write
}

func wireLines(r io.Reader, l Limits) ([]string, error) {
	b, e := readBounded(r, l.BodyBytes)
	if e != nil {
		return nil, e
	}
	if len(b) == 0 || b[len(b)-1] != '\n' || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return nil, ErrProtocol
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	for i, line := range lines {
		if len(line) > l.LineBytes {
			return nil, ErrLimit
		}
		lines[i] = strings.TrimRight(line, "\r")
		if lines[i] == "" {
			return nil, ErrProtocol
		}
	}
	return lines, nil
}

// DecodeRequest validates a complete POST before any store operation can run.
func DecodeRequest(r io.Reader, limits Limits) (Request, error) {
	l, e := limits.normalized()
	if e != nil {
		return Request{}, e
	}
	lines, e := wireLines(r, l)
	if e != nil {
		return Request{}, e
	}
	req := Request{}
	switch lines[0] {
	case "locks/1 get":
	case "locks/1 put":
		req.Put = true
	default:
		return req, ErrProtocol
	}
	if len(lines) < 2 {
		return req, ErrProtocol
	}
	if len(lines)-1 > l.Entries {
		return req, &TooManyError{l.Entries}
	}
	seen := map[ID]bool{}
	for _, line := range lines[1:] {
		w := Write{}
		token := line
		if req.Put {
			verb, rest, ok := strings.Cut(line, " ")
			if !ok {
				return Request{}, ErrProtocol
			}
			w.Mode = Mode(verb)
			token, w.Value, ok = strings.Cut(rest, " ")
			if !ok {
				return Request{}, ErrProtocol
			}
		}
		w.ID, e = ParseKey(token, l)
		if e != nil {
			return Request{}, e
		}
		if seen[w.ID] {
			return Request{}, ErrFormat
		}
		seen[w.ID] = true
		if req.Put {
			w, e = validateWrite(w, l)
			if e != nil {
				return Request{}, e
			}
		}
		req.Items = append(req.Items, w)
	}
	return req, nil
}

// TooManyError reports an advertised batch limit.
type TooManyError struct{ Max int }

func (e *TooManyError) Error() string { return fmt.Sprintf("locks: too-many %d", e.Max) }
func (e *TooManyError) Unwrap() error { return ErrLimit }

// RemoteError retains an unfamiliar, structurally valid peer error code.
type RemoteError struct{ Code string }

func (e *RemoteError) Error() string { return "locks: remote " + e.Code }
func (e *RemoteError) Is(target error) bool {
	switch e.Code {
	case "denied":
		return target == ErrDenied
	case "limit":
		return target == ErrLimit
	case "indeterminate":
		return target == ErrIndeterminate
	case "unsupported":
		return target == ErrUnsupported
	}
	return false
}

// DecodeResults verifies count, order, IDs, mode-specific statuses, and all framing
// before returning any results.
func DecodeResults(r io.Reader, request Request, limits Limits) ([]Outcome, error) {
	l, e := limits.normalized()
	if e != nil {
		return nil, e
	}
	lines, e := wireLines(r, l)
	if e != nil {
		return nil, e
	}
	if lines[0] != "locks/1 results" || len(lines) != len(request.Items)+1 || len(request.Items) == 0 {
		return nil, ErrProtocol
	}
	outcomes := make([]Outcome, len(request.Items))
	for i, line := range lines[1:] {
		status, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, ErrProtocol
		}
		key, value, space := strings.Cut(rest, " ")
		id, e := ParseID(key)
		if e != nil || id != request.Items[i].ID {
			return nil, ErrProtocol
		}
		out := Outcome{Result: Result{ID: id, Status: Status(status)}}
		valid := false
		switch Status(status) {
		case Found:
			valid = !request.Put && space
			if len(value) > l.ValueBytes {
				return nil, ErrLimit
			}
			out.Result.Value = value
		case Missing:
			valid = !request.Put && !space
		case Created, Same:
			valid = request.Put && !space
		case Replaced:
			valid = request.Put && request.Items[i].Mode == Replace && !space
		case Conflict:
			valid = request.Put && request.Items[i].Mode == Create && !space
		case "error":
			valid = space && validName(value) && value != "too-many"
			out.Result.Status = ""
			out.Err = &RemoteError{value}
		}
		if !valid {
			return nil, ErrProtocol
		}
		outcomes[i] = out
	}
	return outcomes, nil
}

// DecodeError accepts only a complete v1 application error. Huge decimal limits
// are validated without integer overflow and capped at the local entry limit.
func DecodeError(r io.Reader, status int, limits Limits) error {
	l, e := limits.normalized()
	if e != nil {
		return e
	}
	lines, e := wireLines(r, l)
	if e != nil {
		return e
	}
	if len(lines) != 2 || lines[0] != "locks/1 error" {
		return ErrProtocol
	}
	code, max, space := strings.Cut(lines[1], " ")
	if !validName(code) {
		return ErrProtocol
	}
	if code != "too-many" {
		if space {
			return ErrProtocol
		}
		return &RemoteError{code}
	}
	if status != 413 || !space || len(max) == 0 || max[0] == '0' {
		return ErrProtocol
	}
	n := 0
	for _, c := range []byte(max) {
		if c < '0' || c > '9' {
			return ErrProtocol
		}
		if n <= l.Entries {
			if n > (l.Entries-int(c-'0'))/10 {
				n = l.Entries + 1
			} else {
				n = n*10 + int(c-'0')
			}
		}
	}
	// The wire minimum is independent of a smaller local limit.
	if len(max) < 2 || len(max) == 2 && max < "16" {
		return ErrProtocol
	}
	if n > l.Entries {
		n = l.Entries
	}
	return &TooManyError{n}
}
func encodeRequest(req Request) string {
	var b strings.Builder
	if req.Put {
		b.WriteString("locks/1 put\n")
	} else {
		b.WriteString("locks/1 get\n")
	}
	for _, w := range req.Items {
		if req.Put {
			fmt.Fprintf(&b, "%s id.%s %s\n", w.Mode, w.ID, w.Value)
		} else {
			fmt.Fprintf(&b, "id.%s\n", w.ID)
		}
	}
	return b.String()
}
func encodeResults(out []Outcome) string {
	var b strings.Builder
	b.WriteString("locks/1 results\n")
	for _, o := range out {
		if o.Err != nil {
			fmt.Fprintf(&b, "error %s %s\n", o.Result.ID, errorCode(o.Err))
			continue
		}
		fmt.Fprintf(&b, "%s %s", o.Result.Status, o.Result.ID)
		if o.Result.Status == Found {
			b.WriteByte(' ')
			b.WriteString(o.Result.Value)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func errorBody(code string, max int) string {
	if code == "too-many" {
		return "locks/1 error\ntoo-many " + strconv.Itoa(max) + "\n"
	}
	return "locks/1 error\n" + code + "\n"
}
