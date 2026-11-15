//go:build darwin || linux || openbsd

package locks

import (
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func openParent(ctx context.Context, path string) (*directory, error) {
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
	return &directory{os.NewFile(uintptr(fd), path), uint64(st.Dev), ctx}, nil
}
func (d *directory) fd() int { return int(d.f.Fd()) }

func (d *directory) reopen() (*os.File, error) {
	fd, err := system.Openat(d.fd(), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "."), nil
}

func (d *directory) node(st *unix.Stat_t) (*nodeInfo, error) {
	if uint64(st.Dev) != d.dev || st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, ErrFormat
	}
	return &nodeInfo{key: identity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, dir: st.Mode&unix.S_IFMT == unix.S_IFDIR, links: uint64(st.Nlink), mode: uint32(st.Mode) & 0777}, nil
}

func (d *directory) rawStat(name string) (*nodeInfo, error) {
	var st unix.Stat_t
	err := system.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d.node(&st)
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
	if e != nil || uint64(st.Dev) != d.dev || dir && st.Mode&unix.S_IFMT != unix.S_IFDIR || !dir && st.Mode&unix.S_IFMT != unix.S_IFREG {
		system.Close(fd)
		if e == nil {
			e = ErrFormat
		}
		return nil, e
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (d *directory) openGuard(name string, create, write bool) (*os.File, *nodeInfo, bool, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
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
		return nil, nil, false, err
	}
	var st unix.Stat_t
	if err = system.Fstat(fd, &st); err != nil {
		return nil, nil, false, errors.Join(err, system.Close(fd))
	}
	info, err := d.node(&st)
	if err != nil {
		return nil, nil, false, errors.Join(err, system.Close(fd))
	}
	return os.NewFile(uintptr(fd), name), info, writable, nil
}

func tryRangeLock(f *os.File, slot int, exclusive bool) error {
	typ := int16(unix.F_RDLCK)
	if exclusive {
		typ = unix.F_WRLCK
	}
	return system.FcntlFlock(f.Fd(), unix.F_SETLK, &unix.Flock_t{Type: typ, Start: int64(slot), Len: 1})
}

func lockBusy(err error) bool {
	return errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EINTR)
}

func unlockRange(f *os.File, slot int) error {
	return system.FcntlFlock(f.Fd(), unix.F_SETLK, &unix.Flock_t{Type: unix.F_UNLCK, Start: int64(slot), Len: 1})
}

func (d *directory) createDir(name string) (*directory, error) {
	if err := system.Mkdirat(d.fd(), name, 0700); err != nil {
		return nil, err
	}
	sub, err := d.child(name)
	if err != nil {
		return nil, err
	}
	if err = clearInheritedACL(sub.f); err != nil {
		return nil, errors.Join(err, sub.f.Close())
	}
	return sub, nil
}

func (d *directory) createFile(name string) (*os.File, error) {
	fd, err := system.Openat(d.fd(), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err = clearInheritedACL(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func (d *directory) rename(from, to string) error {
	return system.Renameat(d.fd(), from, d.fd(), to)
}

func (d *directory) remove(name string, dir bool) error {
	flags := 0
	if dir {
		flags = unix.AT_REMOVEDIR
	}
	return system.Unlinkat(d.fd(), name, flags)
}

func setOwnerMode(src, dst *os.File, mode uint32) error {
	if src == nil {
		return system.Fchmod(int(dst.Fd()), mode)
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
	return system.Fchmod(int(dst.Fd()), mode)
}
