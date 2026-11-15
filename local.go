package locks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// LocalOptions sets permissions and resource bounds. Parent directories must already exist.
type LocalOptions struct {
	ReadOnly bool
	Limits   Limits
	// LockTimeout defaults to 30 seconds. A context may impose a shorter deadline.
	LockTimeout time.Duration
	// FileMode defaults to 0600. Existing ownership, access modes and ACLs are preserved.
	FileMode os.FileMode
}

// Local is a filesystem store. It holds no open handles between operations.
// All processes must use the v1 byte-range locking protocol. Parents must be
// trusted and must not contain links. Configure non-overlapping roots on a coherent filesystem.
type Local struct {
	path, parent, name string
	options            LocalOptions
	limits             Limits
}

// OpenLocal validates configuration without changing the store.
func OpenLocal(path string, options LocalOptions) (*Local, error) {
	if !localSupported {
		return nil, ErrUnsupported
	}
	if strings.ContainsRune(path, 0) {
		return nil, ErrFormat
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: root must be absolute", ErrFormat)
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" && !validWindowsRoot(path) {
		return nil, ErrFormat
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	reserved := name
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		reserved = strings.ToLower(name)
	}
	if path == parent || strings.HasPrefix(reserved, ".") && (strings.HasSuffix(reserved, ".guard") || strings.HasSuffix(reserved, ".tmp") || strings.HasSuffix(reserved, ".old")) {
		return nil, ErrFormat
	}
	l, e := options.Limits.normalized()
	if e != nil {
		return nil, e
	}
	if options.LockTimeout < 0 {
		return nil, ErrLimit
	}
	if options.LockTimeout == 0 {
		options.LockTimeout = 30 * time.Second
	}
	if options.FileMode == 0 {
		options.FileMode = 0600
	}
	if options.FileMode&^0777 != 0 {
		return nil, ErrFormat
	}
	return &Local{path, parent, name, options, l}, nil
}
func validWindowsName(name string) bool {
	// Windows versions differ on whether device names with extensions are
	// reserved. Check the base name separately to reject them on every version.
	base, _, _ := strings.Cut(name, ".")
	base = strings.TrimRight(base, " ")
	if base != "" && !filepath.IsLocal(base) {
		return false
	}
	return filepath.IsLocal(name) && name != "." && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ") &&
		!strings.ContainsAny(name, `\/:"<>|?*`) && strings.IndexFunc(name, func(r rune) bool { return r < 32 }) == -1
}

func validWindowsRoot(path string) bool {
	if !validLocation(path, "windows") {
		return false
	}
	parts := strings.TrimPrefix(path[len(filepath.VolumeName(path)):], `\`)
	if parts == "" {
		return false
	}
	for _, name := range strings.Split(parts, `\`) {
		if !validWindowsName(name) {
			return false
		}
	}
	return true
}

func (s *Local) Get(ctx context.Context, id ID) (Result, error) {
	if _, e := ParseID(string(id)); e != nil {
		return Result{}, e
	}
	return s.operation(ctx, id, nil)
}
func (s *Local) Put(ctx context.Context, id ID, value string, mode Mode) (Result, error) {
	if s.options.ReadOnly {
		return Result{}, ErrDenied
	}
	w, e := validateWrite(Write{id, value, mode}, s.limits)
	if e != nil {
		return Result{}, e
	}
	return s.operation(ctx, id, &w)
}

// Validate checks the entire reachable tree under an exclusive root lock. It does
// not change recovery sidecars. Read-only permissions on the guard may prevent it.
func (s *Local) Validate(ctx context.Context) error { return s.maintenance(ctx, false) }

// Recover restores interrupted publications and validates the resulting tree.
func (s *Local) Recover(ctx context.Context) error {
	if s.options.ReadOnly {
		return ErrDenied
	}
	return s.maintenance(ctx, true)
}
