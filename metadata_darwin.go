//go:build darwin

package locks

import (
	"encoding/binary"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin exposes native ACLs through ATTR_CMN_EXTENDED_SECURITY, separately
// from user xattrs. Use descriptor-based calls for both files and directories.
func aclAttr(fd uintptr, trap uintptr, buf []byte, options uintptr) error {
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	if trap == unix.SYS_FGETATTRLIST {
		attrs.Commonattr |= unix.ATTR_CMN_RETURNED_ATTRS
	}
	_, _, errno := unix.Syscall6(trap, fd, uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), options, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// From <sys/kauth.h>: a kauth_filesec header contains magic, two GUIDs,
// entry count and flags. Each ACE contains a GUID, flags and access rights.
const (
	filesecMagic  = 0x012cc16d
	noACL         = ^uint32(0)
	filesecHeader = 44
	aceSize       = 24
	maxACEs       = 128
)

func readACL(f *os.File) ([]byte, error) {
	// A length and five returned attribute bitmaps precede the attrreference.
	// Its data offset is relative to the start of the reference (byte 24).
	buf := make([]byte, 32+filesecHeader+aceSize*maxACEs)
	if err := aclAttr(f.Fd(), unix.SYS_FGETATTRLIST, buf, unix.FSOPT_REPORT_FULLSIZE|unix.FSOPT_PACK_INVAL_ATTRS); err != nil {
		return nil, err
	}
	u32 := binary.NativeEndian.Uint32
	total, offset, n := int(u32(buf)), 24+int(u32(buf[24:])), int(u32(buf[28:]))
	if total < 32 || total > len(buf) {
		return nil, ErrFormat
	}
	if u32(buf[4:])&unix.ATTR_CMN_EXTENDED_SECURITY == 0 || n == 0 {
		return nil, nil
	}
	if offset < 32 || n < filesecHeader || offset+n > total {
		return nil, ErrFormat
	}
	acl := buf[offset : offset+n]
	count := u32(acl[36:])
	if u32(acl) != filesecMagic {
		return nil, ErrFormat
	}
	if count == noACL && n == filesecHeader {
		return nil, nil
	}
	if count > maxACEs || filesecHeader+int(count)*aceSize != n {
		return nil, ErrFormat
	}
	return acl, nil
}

func writeACL(f *os.File, acl []byte) error {
	if acl == nil {
		acl = make([]byte, filesecHeader)
		binary.NativeEndian.PutUint32(acl, filesecMagic)
		binary.NativeEndian.PutUint32(acl[36:], noACL)
	}
	buf := make([]byte, 8+len(acl))
	binary.NativeEndian.PutUint32(buf, 8)
	binary.NativeEndian.PutUint32(buf[4:], uint32(len(acl)))
	copy(buf[8:], acl)
	return aclAttr(f.Fd(), unix.SYS_FSETATTRLIST, buf, 0)
}

func clearInheritedACL(f *os.File) error {
	acl, err := readACL(f)
	if errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil || acl == nil {
		return err
	}
	return writeACL(f, nil)
}

func setMetadata(src, dst *os.File, mode uint32, dir bool) error {
	if err := setOwnerMode(src, dst, mode); err != nil || src == nil {
		return err
	}
	acl, err := readACL(src)
	if err != nil && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	if len(acl) > 0 {
		if dir {
			for at := filesecHeader; at < len(acl); at += aceSize {
				flags := binary.NativeEndian.Uint32(acl[at+16:])
				rights := binary.NativeEndian.Uint32(acl[at+20:])
				// Map both allow and deny read-data rights to directory search.
				if (flags&0xf == 1 || flags&0xf == 2) && rights&(1<<1) != 0 {
					binary.NativeEndian.PutUint32(acl[at+20:], rights|1<<3)
				}
			}
		}
		if err = writeACL(dst, acl); err != nil {
			return err
		}
	}
	return copyXattrs(src, dst, dir)
}

func skipXattr(name string) bool        { return name == "com.apple.system.Security" }
func prepareXattr(string, []byte, bool) {}
