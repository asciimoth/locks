//go:build darwin || linux

package locks

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func copyXattrs(src, dst *os.File, dir bool) error {
	n, e := xattrs.Flistxattr(int(src.Fd()), nil)
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
	n, e = xattrs.Flistxattr(int(src.Fd()), names)
	if e != nil {
		return e
	}
	for _, name := range strings.Split(string(names[:n]), "\x00") {
		if name == "" || skipXattr(name) {
			continue
		}
		size, e := xattrs.Fgetxattr(int(src.Fd()), name, nil)
		if e != nil {
			return e
		}
		v := make([]byte, size)
		size, e = xattrs.Fgetxattr(int(src.Fd()), name, v)
		if e != nil {
			return e
		}
		prepareXattr(name, v[:size], dir)
		if e = xattrs.Fsetxattr(int(dst.Fd()), name, v[:size], 0); e != nil {
			return e
		}
	}
	return nil
}
