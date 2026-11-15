//go:build darwin || linux

package locks

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func mergeExtraMetadata(f *os.File, directory bool, out mergeAccess) (mergeAccess, error) {
	var err error
	out.ACL, err = mergeACL(f, directory)
	if err != nil {
		return out, err
	}
	fd := int(f.Fd())
	n, err := xattrs.Flistxattr(fd, nil)
	if errors.Is(err, unix.ENOTSUP) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if n < 0 || n > 1<<20 {
		return out, ErrLimit
	}
	names := make([]byte, n)
	n, err = xattrs.Flistxattr(fd, names)
	if err != nil {
		return out, err
	}
	if n < 0 || n > len(names) {
		return out, ErrFormat
	}
	out.Attributes = map[string]string{}
	for _, name := range strings.Split(string(names[:n]), "\x00") {
		if name == "" || skipXattr(name) {
			continue
		}
		n, err := xattrs.Fgetxattr(fd, name, nil)
		if err != nil {
			return out, err
		}
		if n < 0 || n > 1<<20 {
			return out, ErrLimit
		}
		value := make([]byte, n)
		n, err = xattrs.Fgetxattr(fd, name, value)
		if err != nil {
			return out, err
		}
		if n < 0 || n > len(value) {
			return out, ErrFormat
		}
		prepareXattr(name, value[:n], directory)
		out.Attributes[name] = string(value[:n])
	}
	return out, nil
}
