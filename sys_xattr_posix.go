//go:build darwin || linux

package locks

import "golang.org/x/sys/unix"

var xattrs = struct {
	Flistxattr   func(int, []byte) (int, error)
	Fgetxattr    func(int, string, []byte) (int, error)
	Fsetxattr    func(int, string, []byte, int) error
	Fremovexattr func(int, string) error
}{unix.Flistxattr, unix.Fgetxattr, unix.Fsetxattr, unix.Fremovexattr}
