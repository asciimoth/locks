//go:build darwin || linux || openbsd

package locks

import "golang.org/x/sys/unix"

// Keep system calls at one boundary so fault tests can exercise failures at each
// publication step. Production code never replaces this table.
var system = struct {
	Open       func(string, int, uint32) (int, error)
	Openat     func(int, string, int, uint32) (int, error)
	Close      func(int) error
	Fstat      func(int, *unix.Stat_t) error
	Fstatat    func(int, string, *unix.Stat_t, int) error
	FcntlFlock func(uintptr, int, *unix.Flock_t) error
	Unlinkat   func(int, string, int) error
	Renameat   func(int, string, int, string) error
	Mkdirat    func(int, string, uint32) error
	Fchown     func(int, int, int) error
	Fchmod     func(int, uint32) error
}{unix.Open, unix.Openat, unix.Close, unix.Fstat, unix.Fstatat, unix.FcntlFlock, unix.Unlinkat, unix.Renameat, unix.Mkdirat, unix.Fchown, unix.Fchmod}
