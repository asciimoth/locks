//go:build linux

package locks

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// setMetadata preserves ownership, access modes and Linux extended attributes,
// including POSIX ACLs. A failure leaves the live leaf in place.
func setMetadata(src, dst *os.File, mode uint32, dir bool) error {
	if src == nil {
		return setOwnerMode(nil, dst, mode)
	}
	// Creation can inherit an ACL that the source did not have. Remove it
	// before chmod can activate its masked permissions, then copy source ACLs.
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		e := xattrs.Fremovexattr(int(dst.Fd()), name)
		if e != nil && !errors.Is(e, unix.ENODATA) && !errors.Is(e, unix.ENOTSUP) {
			return e
		}
	}
	if err := setOwnerMode(src, dst, mode); err != nil {
		return err
	}
	return copyXattrs(src, dst, dir)
}

func skipXattr(string) bool { return false }
func prepareXattr(name string, v []byte, dir bool) {
	if name == "system.posix_acl_access" && dir {
		// Linux ACL xattrs have a four-byte header and eight-byte entries.
		for i := 4; i+8 <= len(v); i += 8 {
			if v[i+2]&4 != 0 {
				v[i+2] |= 1
			}
		}
	}
}
