package locks

import (
	"context"
	"errors"
	"io"
	"reflect"
)

// An interface can contain a nil pointer, map, slice, function or channel.
func nilInput(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

func contextError(ctx context.Context) error {
	if nilInput(ctx) {
		return ErrFormat
	}
	return ctx.Err()
}

// Check reader counts before io.ReadAll uses them as slice bounds. Stop a
// reader that repeatedly returns no bytes and no error, as bufio does.
type checkedReader struct {
	r     io.Reader
	empty int
}

func (r *checkedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n < 0 || n > len(p) {
		return 0, errors.Join(ErrFormat, err)
	}
	if n == 0 && err == nil {
		r.empty++
		if r.empty >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		r.empty = 0
	}
	return n, err
}
