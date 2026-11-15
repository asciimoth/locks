package locks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Error codes can be checked with errors.Is, including errors from remote stores.
var (
	ErrFormat          = errors.New("locks: format")
	ErrLimit           = errors.New("locks: limit")
	ErrUnsupported     = errors.New("locks: unsupported")
	ErrDenied          = errors.New("locks: denied")
	ErrProtocol        = errors.New("locks: protocol")
	ErrIndeterminate   = errors.New("locks: indeterminate")
	ErrNoSource        = errors.New("locks: no-source")
	ErrNoWritableStore = errors.New("locks: no-writable-store")
)

// Mode selects the condition for a write.
type Mode string

const (
	Create  Mode = "create"
	Replace Mode = "replace"
)

// Status is an operation's semantic result. Conflict is not an operational error.
type Status string

const (
	Found    Status = "found"
	Missing  Status = "missing"
	Created  Status = "created"
	Same     Status = "same"
	Replaced Status = "replaced"
	Conflict Status = "conflict"
)

// Result distinguishes an empty value from an absent key.
type Result struct {
	ID     ID
	Status Status
	Value  string
}

// Store serializes each operation for its key. Methods accept canonical IDs.
type Store interface {
	Get(context.Context, ID) (Result, error)
	Put(context.Context, ID, string, Mode) (Result, error)
}

// Write is one batch write. A batch is not a transaction.
type Write struct {
	ID    ID
	Value string
	Mode  Mode
}

// Outcome is one batch result.
type Outcome struct {
	Result Result
	Err    error
}

// Limits bounds untrusted data. Zero fields select defaults; negative fields are invalid.
type Limits struct{ RawKeyBytes, ValueBytes, LineBytes, ScanBytes, Entries, BodyBytes, TargetBytes int }

func (l Limits) normalized() (Limits, error) {
	p := []*int{&l.RawKeyBytes, &l.ValueBytes, &l.LineBytes, &l.ScanBytes, &l.Entries, &l.BodyBytes, &l.TargetBytes}
	d := []int{1 << 20, 1 << 20, 2 << 20, 64 << 20, 1024, 16 << 20, 16 << 10}
	for i, v := range p {
		if *v < 0 || *v > int(^uint(0)>>1)/4 {
			return l, fmt.Errorf("%w: invalid budget", ErrLimit)
		}
		if *v == 0 {
			*v = d[i]
		}
	}
	return l, nil
}

// NormalizeValue validates UTF-8 and removes all trailing CR bytes.
func NormalizeValue(s string) (string, error) {
	if !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\n") {
		return "", fmt.Errorf("%w: value", ErrFormat)
	}
	return strings.TrimRight(s, "\r"), nil
}
func validateWrite(w Write, l Limits) (Write, error) {
	if _, e := ParseID(string(w.ID)); e != nil {
		return w, e
	}
	if w.Mode != Create && w.Mode != Replace {
		return w, fmt.Errorf("%w: write mode", ErrFormat)
	}
	v, e := NormalizeValue(w.Value)
	if e != nil {
		return w, e
	}
	w.Value = v
	if len(v) > l.ValueBytes {
		return w, ErrLimit
	}
	return w, nil
}
func decide(id ID, old string, exists bool, w Write) Result {
	r := Result{ID: id, Status: Created}
	if exists {
		r.Status = Same
		if old != w.Value {
			if w.Mode == Create {
				r.Status = Conflict
			} else {
				r.Status = Replaced
			}
		}
	}
	return r
}
func validName(s string) bool {
	if len(s) < 1 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range []byte(s[1:]) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
