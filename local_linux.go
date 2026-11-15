//go:build linux

package locks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const localSupported = true

// All namespace operations are relative to open directories. O_NOFOLLOW applies
// to each component, including parent resolution. No preflight path check is used.
type directory struct {
	f   *os.File
	dev uint64
}

func openParent(path string) (*directory, error) {
	fd, e := system.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	for _, p := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if p == "" {
			continue
		}
		next, err := system.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		ce := system.Close(fd)
		if err != nil {
			return nil, err
		}
		if ce != nil {
			system.Close(next)
			return nil, ce
		}
		fd = next
	}
	var st unix.Stat_t
	if e = system.Fstat(fd, &st); e != nil {
		system.Close(fd)
		return nil, e
	}
	return &directory{os.NewFile(uintptr(fd), path), st.Dev}, nil
}
func (d *directory) fd() int { return int(d.f.Fd()) }

// checkName rejects case aliases, including an alias beside an exact match.
// Use a new directory handle so repeated checks and concurrent readers do not
// share an enumeration offset. Check only this directory, not its descendants.
// As with other namespace checks, external edits must stop during operations.
func (d *directory) checkName(name string) error {
	return d.checkNameRetry(name, true)
}

func (d *directory) checkNameRetry(name string, retry bool) (err error) {
	fd, e := system.Openat(d.fd(), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(fd), ".")
	defer func() { err = errors.Join(err, f.Close()) }()
	exact := false
	for {
		names, e := f.Readdirnames(128)
		for _, actual := range names {
			exact = exact || actual == name
			if actual != name && strings.EqualFold(actual, name) {
				return fmt.Errorf("%w: case alias %q for %q", ErrFormat, actual, name)
			}
		}
		if errors.Is(e, io.EOF) {
			if !exact {
				// Filesystems can equate names beyond Unicode simple case
				// folding. Never accept such an alias as the requested entry.
				var st unix.Stat_t
				e = system.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW)
				if e == nil {
					// Another process can create the guard after the scan.
					// Read fresh entries before reporting an alias.
					if retry {
						return d.checkNameRetry(name, false)
					}
					return fmt.Errorf("%w: no exact entry for %q", ErrFormat, name)
				}
				if !errors.Is(e, unix.ENOENT) {
					return e
				}
			}
			return nil
		}
		if e != nil {
			return e
		}
	}
}

func (d *directory) stat(name string) (*unix.Stat_t, error) {
	if e := d.checkName(name); e != nil {
		return nil, e
	}
	var st unix.Stat_t
	e := system.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(e, unix.ENOENT) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if st.Dev != d.dev || st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("%w: unsafe entry %s", ErrFormat, name)
	}
	return &st, nil
}
func (d *directory) open(name string, dir bool) (*os.File, error) {
	if e := d.checkName(name); e != nil {
		return nil, e
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if dir {
		flags |= unix.O_DIRECTORY
	}
	fd, e := system.Openat(d.fd(), name, flags, 0)
	if e != nil {
		return nil, e
	}
	var st unix.Stat_t
	e = system.Fstat(fd, &st)
	if e != nil || st.Dev != d.dev || dir && st.Mode&unix.S_IFMT != unix.S_IFDIR || !dir && st.Mode&unix.S_IFMT != unix.S_IFREG {
		system.Close(fd)
		if e == nil {
			e = ErrFormat
		}
		return nil, e
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (d *directory) child(name string) (*directory, error) {
	f, e := d.open(name, true)
	if e != nil {
		return nil, e
	}
	return &directory{f, d.dev}, nil
}
func (d *directory) leaf(name, p string, l Limits) (leaf *Leaf, err error) {
	f, e := d.open(name, false)
	if e != nil {
		return nil, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return DecodeLeaf(f, p, l)
}

type identity struct{ dev, ino uint64 }
type rangeOwner struct {
	readers int
	writer  bool
}
type guard struct {
	f        *os.File
	key      identity
	refs     int
	mu       sync.Mutex
	ranges   [1025]rangeOwner
	writable bool
}

var guards = struct {
	sync.Mutex
	m map[identity]*guard
}{m: map[identity]*guard{}}

func getGuard(d *directory, name string, write bool) (*guard, error) {
	guards.Lock()
	defer guards.Unlock()
	if e := d.checkName(name); e != nil {
		return nil, e
	}
	var st unix.Stat_t
	e := system.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(e, unix.ENOENT) && !write {
		return nil, nil
	}
	if e != nil && !errors.Is(e, unix.ENOENT) {
		return nil, e
	}
	if e == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Dev != d.dev {
			return nil, ErrFormat
		}
		key := identity{st.Dev, st.Ino}
		if g := guards.m[key]; g != nil {
			if write && !g.writable {
				return nil, ErrDenied
			}
			g.refs++
			return g, nil
		}
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if errors.Is(e, unix.ENOENT) {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := system.Openat(d.fd(), name, flags, 0600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = system.Openat(d.fd(), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	}
	writable := true
	if err != nil && !write && (errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS)) {
		fd, err = system.Openat(d.fd(), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		writable = false
	}
	if err != nil {
		return nil, err
	}
	if err = system.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Dev != d.dev {
		system.Close(fd)
		if err == nil {
			err = ErrFormat
		}
		return nil, err
	}
	g := &guard{f: os.NewFile(uintptr(fd), name), key: identity{st.Dev, st.Ino}, refs: 1, writable: writable}
	guards.m[g.key] = g
	return g, nil
}
func (g *guard) close() error {
	guards.Lock()
	defer guards.Unlock()
	g.refs--
	if g.refs == 0 {
		delete(guards.m, g.key)
		return g.f.Close()
	}
	return nil
}
func (g *guard) lock(ctx context.Context, slot int, exclusive bool) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		g.mu.Lock()
		r := &g.ranges[slot]
		if !r.writer && (!exclusive || r.readers == 0) {
			var e error
			if r.readers == 0 {
				typ := int16(unix.F_RDLCK)
				if exclusive {
					typ = unix.F_WRLCK
				}
				e = system.FcntlFlock(g.f.Fd(), unix.F_SETLK, &unix.Flock_t{Type: typ, Whence: 0, Start: int64(slot), Len: 1})
			}
			if e == nil {
				if exclusive {
					r.writer = true
				} else {
					r.readers++
				}
				g.mu.Unlock()
				return nil
			}
			if !errors.Is(e, unix.EAGAIN) && !errors.Is(e, unix.EACCES) && !errors.Is(e, unix.EINTR) {
				g.mu.Unlock()
				return e
			}
		}
		g.mu.Unlock()
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (g *guard) unlock(slot int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := &g.ranges[slot]
	if r.writer {
		r.writer = false
	} else {
		r.readers--
	}
	if r.readers > 0 {
		return nil
	}
	return system.FcntlFlock(g.f.Fd(), unix.F_SETLK, &unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: int64(slot), Len: 1})
}
func slotFor(id ID) int {
	p := prefix(id)
	return 1 + 32*strings.IndexByte(alphabet, p[0]) + strings.IndexByte(alphabet, p[1])
}
func sidecars(d *directory, name string) (bool, error) {
	for _, suffix := range []string{".old", ".tmp"} {
		s, e := d.stat("." + name + suffix)
		if e != nil {
			return false, e
		}
		if s != nil {
			return true, nil
		}
	}
	return false, nil
}
func (s *Local) operation(ctx context.Context, id ID, w *Write) (result Result, err error) {
	defer func() {
		if err != nil && w != nil && (result.Status == Created || result.Status == Replaced) {
			err = errors.Join(ErrIndeterminate, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, s.options.LockTimeout)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return Result{}, e
	}
	d, e := openParent(s.parent)
	if e != nil {
		return Result{}, e
	}
	defer func() { err = errors.Join(err, d.f.Close()) }()
	for {
		g, e := getGuard(d, "."+s.name+".guard", w != nil)
		if e != nil {
			return Result{}, e
		}
		if g == nil {
			// An immutable checkout can be read without write access to its parent.
			r, e := s.lookup(d, s.name, "", id, nil, false)
			again, ge := getGuard(d, "."+s.name+".guard", false)
			if ge != nil {
				return Result{}, ge
			}
			if again == nil {
				return r, e
			}
			if ce := again.close(); ce != nil {
				return Result{}, ce
			}
			continue
		}
		r, e := s.guarded(ctx, d, g, id, w)
		e = errors.Join(e, g.close())
		return r, e
	}
}
func (s *Local) guarded(ctx context.Context, d *directory, g *guard, id ID, w *Write) (r Result, err error) {
	if e := g.lock(ctx, 0, false); e != nil {
		return r, e
	}
	rootHeld := true
	defer func() {
		if rootHeld {
			err = errors.Join(err, g.unlock(0))
		}
	}()
	st, e := d.stat(s.name)
	if e != nil {
		return r, e
	}
	sides, e := sidecars(d, s.name)
	if e != nil {
		return r, e
	}
	if w != nil && (st == nil || st.Mode&unix.S_IFMT == unix.S_IFREG || sides) {
		rootHeld = false
		if e = g.unlock(0); e != nil {
			return r, e
		}
		if e = g.lock(ctx, 0, true); e != nil {
			return r, e
		}
		rootHeld = true
		if e = s.recoverNode(d, s.name, ""); e != nil {
			return r, e
		}
		st, e = d.stat(s.name)
		if e != nil {
			return r, e
		}
		if st == nil || st.Mode&unix.S_IFMT == unix.S_IFREG {
			return s.lookup(d, s.name, "", id, w, true)
		}
		// Release exclusive ownership before resuming the normal root/slot order.
		rootHeld = false
		if e = g.unlock(0); e != nil {
			return r, e
		}
		return s.guarded(ctx, d, g, id, w)
	}
	if st != nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
		slot := slotFor(id)
		if e = g.lock(ctx, slot, w != nil); e != nil {
			return r, e
		}
		defer func() { err = errors.Join(err, g.unlock(slot)) }()
	}
	return s.lookup(d, s.name, "", id, w, true)
}
func (s *Local) lookup(d *directory, name, p string, id ID, w *Write, guarded bool) (r Result, err error) {
	if w != nil && p != "" {
		if e := s.recoverNode(d, name, p); e != nil {
			return r, e
		}
	}
	st, e := d.stat(name)
	if e != nil {
		return r, e
	}
	old, e := d.stat("." + name + ".old")
	if e != nil {
		return r, e
	}
	tmp, e := d.stat("." + name + ".tmp")
	if e != nil {
		return r, e
	}
	if !guarded && (old != nil || tmp != nil) {
		return r, fmt.Errorf("%w: sidecars without guard", ErrFormat)
	}
	if old != nil && (old.Mode&unix.S_IFMT != unix.S_IFREG || old.Nlink != 1) {
		return r, ErrFormat
	}
	live := name
	if st == nil && old != nil {
		st = old
		live = "." + name + ".old"
	}
	if st != nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if len(p) >= 52 {
			return r, ErrFormat
		}
		child, e := d.child(live)
		if e != nil {
			return r, e
		}
		defer func() { err = errors.Join(err, child.f.Close()) }()
		c := component(prefix(id), len(p))
		return s.lookup(child, c, p+c, id, w, guarded)
	}
	leaf := &Leaf{}
	if st != nil {
		leaf, e = d.leaf(live, p, s.limits)
		if e != nil {
			return r, e
		}
	}
	if w == nil {
		r = Result{ID: id, Status: Missing}
		for _, rec := range leaf.Records {
			if rec.ID == id {
				r.Status = Found
				r.Value = rec.Value
			}
		}
		return r, nil
	}
	r, data, records := leaf.edit(*w)
	if r.Status == Same || r.Status == Conflict {
		return r, nil
	}
	if st != nil && st.Nlink != 1 {
		return Result{}, fmt.Errorf("%w: multiply linked leaf", ErrFormat)
	}
	if e = s.publish(d, name, p, data, records, st); e != nil {
		return Result{}, e
	}
	return r, nil
}

// inspectTree checks staged containment before deletion. It does not interpret
// stage contents: an incomplete stage is never authoritative.
func inspectTree(d *directory, name string) (err error) {
	st, e := d.stat(name)
	if e != nil || st == nil {
		return e
	}
	if st.Mode&unix.S_IFMT == unix.S_IFREG {
		if st.Nlink != 1 {
			return ErrFormat
		}
		return nil
	}
	sub, e := d.child(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	entries, e := sub.f.ReadDir(-1)
	if e != nil {
		return e
	}
	for _, en := range entries {
		if e = inspectTree(sub, en.Name()); e != nil {
			return e
		}
	}
	return nil
}
func removeTree(d *directory, name string) (err error) {
	st, e := d.stat(name)
	if e != nil || st == nil {
		return e
	}
	if st.Mode&unix.S_IFMT == unix.S_IFREG {
		return system.Unlinkat(d.fd(), name, 0)
	}
	sub, e := d.child(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	entries, e := sub.f.ReadDir(-1)
	if e != nil {
		return e
	}
	for _, en := range entries {
		if e = removeTree(sub, en.Name()); e != nil {
			return e
		}
	}
	return system.Unlinkat(d.fd(), name, unix.AT_REMOVEDIR)
}
func (s *Local) recoverNode(d *directory, name, p string) error {
	oldName, tmpName := "."+name+".old", "."+name+".tmp"
	st, e := d.stat(name)
	if e != nil {
		return e
	}
	old, e := d.stat(oldName)
	if e != nil {
		return e
	}
	tmp, e := d.stat(tmpName)
	if e != nil {
		return e
	}
	if old != nil && (old.Mode&unix.S_IFMT != unix.S_IFREG || old.Nlink != 1) {
		return ErrFormat
	}
	if tmp != nil {
		if e = inspectTree(d, tmpName); e != nil {
			return e
		}
	}
	if old != nil {
		if st != nil {
			if e = s.validateNode(d, name, p, false); e != nil {
				return e
			}
		} else {
			if _, e = d.leaf(oldName, p, s.limits); e != nil {
				return e
			}
			if e = system.Renameat(d.fd(), oldName, d.fd(), name); e != nil {
				return e
			}
			old = nil
		}
	}
	if old != nil {
		if e = system.Unlinkat(d.fd(), oldName, 0); e != nil {
			return e
		}
	}
	if tmp != nil {
		return removeTree(d, tmpName)
	}
	return nil
}
func (s *Local) validateNode(d *directory, name, p string, recover bool) (err error) {
	for _, suffix := range []string{".tmp", ".old"} {
		side := "." + name + suffix
		st, e := d.stat(side)
		if e != nil {
			return e
		}
		if st != nil {
			if suffix == ".old" && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1) {
				return ErrFormat
			}
			if e = inspectTree(d, side); e != nil {
				return e
			}
		}
	}
	if recover {
		if e := s.recoverNode(d, name, p); e != nil {
			return e
		}
	}
	st, e := d.stat(name)
	if e != nil {
		return e
	}
	if st == nil {
		old, e := d.stat("." + name + ".old")
		if e != nil {
			return e
		}
		if old == nil {
			return nil
		}
		if old.Mode&unix.S_IFMT != unix.S_IFREG || old.Nlink != 1 {
			return ErrFormat
		}
		_, e = d.leaf("."+name+".old", p, s.limits)
		return e
	}
	if st.Mode&unix.S_IFMT == unix.S_IFREG {
		_, e = d.leaf(name, p, s.limits)
		return e
	}
	if len(p) >= 52 {
		return ErrFormat
	}
	sub, e := d.child(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	entries, e := sub.f.ReadDir(-1)
	if e != nil {
		return e
	}
	names := map[string]bool{}
	for _, en := range entries {
		n := en.Name()
		base, side, e := shardName(n, len(p))
		if e != nil {
			return e
		}
		if side != "" {
			if e = inspectTree(sub, n); e != nil {
				return e
			}
			if side == "old" {
				st, e := sub.stat(n)
				if e != nil {
					return e
				}
				if st.Mode&unix.S_IFMT != unix.S_IFREG {
					return ErrFormat
				}
			}
		}
		names[base] = true
	}
	for n := range names {
		if e = s.validateNode(sub, n, p+n, recover); e != nil {
			return e
		}
	}
	return nil
}
func (s *Local) maintenance(ctx context.Context, recover bool) (err error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.LockTimeout)
	defer cancel()
	d, e := openParent(s.parent)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, d.f.Close()) }()
	g, e := getGuard(d, "."+s.name+".guard", true)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, g.close()) }()
	if e = g.lock(ctx, 0, true); e != nil {
		return e
	}
	defer func() { err = errors.Join(err, g.unlock(0)) }()
	if recover {
		if e = s.recoverNode(d, s.name, ""); e != nil {
			return e
		}
	} else {
		if _, e = sidecars(d, s.name); e != nil {
			return e
		}
	}
	return s.validateNode(d, s.name, "", recover)
}

// copyMetadata preserves ownership, access modes and Linux extended attributes,
// including POSIX ACLs. A failure leaves the live leaf in place.
func copyMetadata(src, dst *os.File, mode uint32) error {
	// Creation can inherit an ACL that the source did not have. Remove it
	// before chmod can activate its masked permissions, then copy source ACLs.
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		e := system.Fremovexattr(int(dst.Fd()), name)
		if e != nil && !errors.Is(e, unix.ENODATA) && !errors.Is(e, unix.ENOTSUP) {
			return e
		}
	}
	var st unix.Stat_t
	if e := system.Fstat(int(src.Fd()), &st); e != nil {
		return e
	}
	var target unix.Stat_t
	if e := system.Fstat(int(dst.Fd()), &target); e != nil {
		return e
	}
	if st.Uid != target.Uid || st.Gid != target.Gid {
		if e := system.Fchown(int(dst.Fd()), int(st.Uid), int(st.Gid)); e != nil {
			return e
		}
	}
	if e := system.Fchmod(int(dst.Fd()), mode); e != nil {
		return e
	}
	n, e := system.Flistxattr(int(src.Fd()), nil)
	if errors.Is(e, unix.ENOTSUP) {
		return nil
	}
	if e != nil {
		return e
	}
	if n == 0 {
		return nil
	}
	names := make([]byte, n)
	n, e = system.Flistxattr(int(src.Fd()), names)
	if e != nil {
		return e
	}
	for _, name := range strings.Split(string(names[:n]), "\x00") {
		if name == "" {
			continue
		}
		size, e := system.Fgetxattr(int(src.Fd()), name, nil)
		if e != nil {
			return e
		}
		v := make([]byte, size)
		size, e = system.Fgetxattr(int(src.Fd()), name, v)
		if e != nil {
			return e
		}
		if name == "system.posix_acl_access" && target.Mode&unix.S_IFMT == unix.S_IFDIR {
			// Linux ACL xattrs have a four-byte header and eight-byte entries.
			for i := 4; i+8 <= size; i += 8 {
				if v[i+2]&4 != 0 {
					v[i+2] |= 1
				}
			}
		}
		if e = system.Fsetxattr(int(dst.Fd()), name, v[:size], 0); e != nil {
			return e
		}
	}
	return nil
}
func (s *Local) stage(d *directory, name, p string, data []byte, records []Record, source *os.File, mode uint32) (err error) {
	split := len(records) > 512 && len(p) < 52
	if !split {
		if data == nil {
			var b strings.Builder
			if e := EncodeLeaf(&b, records); e != nil {
				return e
			}
			data = []byte(b.String())
		}
		if _, e := DecodeLeaf(strings.NewReader(string(data)), p, s.limits); e != nil {
			return e
		}
	}
	if split {
		if e := system.Mkdirat(d.fd(), name, 0700); e != nil {
			return e
		}
		sub, e := d.child(name)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, sub.f.Close()) }()
		groups := map[string][]Record{}
		var order []string
		for _, r := range records {
			c := component(prefix(r.ID), len(p))
			if _, ok := groups[c]; !ok {
				order = append(order, c)
			}
			groups[c] = append(groups[c], r)
		}
		for _, c := range order {
			if e = s.stage(sub, c, p+c, nil, groups[c], source, mode); e != nil {
				return e
			}
		}
		dirMode := mode | ((mode & 0444) >> 2)
		if source != nil {
			return copyMetadata(source, sub.f, dirMode)
		}
		return system.Fchmod(sub.fd(), dirMode)
	}
	fd, e := system.Openat(d.fd(), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { err = errors.Join(err, f.Close()) }()
	if data != nil {
		_, e = f.Write(data)
	} else {
		e = EncodeLeaf(f, records)
	}
	if e != nil {
		return e
	}
	if source != nil {
		return copyMetadata(source, f, mode)
	}
	return system.Fchmod(fd, mode)
}
func (s *Local) publish(d *directory, name, p string, data []byte, records []Record, st *unix.Stat_t) (err error) {
	tmp, old := "."+name+".tmp", "."+name+".old"
	mode := uint32(s.options.FileMode.Perm())
	var source *os.File
	if st != nil {
		mode = st.Mode & 0777
		f, e := d.open(name, false)
		if e != nil {
			return e
		}
		source = f
	}
	stageErr := s.stage(d, tmp, p, data, records, source, mode)
	if source != nil {
		stageErr = errors.Join(stageErr, source.Close())
	}
	if stageErr != nil {
		return stageErr
	}
	// Always use the backup protocol, for file edits as well as splits.
	if st != nil {
		if e := system.Renameat(d.fd(), name, d.fd(), old); e != nil {
			return e
		}
	}
	if e := system.Renameat(d.fd(), tmp, d.fd(), name); e != nil {
		return e
	}
	if st != nil {
		if e := system.Unlinkat(d.fd(), old, 0); e != nil {
			return errors.Join(ErrIndeterminate, e)
		}
	}
	return nil
}
