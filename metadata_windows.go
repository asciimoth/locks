//go:build windows

package locks

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Create the guard and staging objects with a protected DACL before writing
// any bytes. In particular a permissive inheritable ACE on the parent must
// never expose a staged copy of a private leaf.
func privateDescriptor(parent *os.File) (*windows.SECURITY_DESCRIPTOR, error) {
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(windows.Handle(parent.Fd()), nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return nil, err
	}
	if flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return nil, fmt.Errorf("%w: volume lacks persistent ACLs", ErrUnsupported)
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
}

// FileMode controls new entries. Express owner, primary group and other bits
// as NT access masks. Writing a leaf also permits its rename during publication.
func modeDescriptor(sd *windows.SECURITY_DESCRIPTOR, mode uint32) (*windows.SECURITY_DESCRIPTOR, error) {
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	group, _, err := sd.Group()
	if err != nil {
		return nil, err
	}
	var masks [3]uint32
	for i := range masks {
		bits := (mode >> (6 - 3*i)) & 7
		if bits&4 != 0 {
			masks[i] |= windows.FILE_GENERIC_READ
		}
		if bits&2 != 0 {
			masks[i] |= windows.FILE_GENERIC_WRITE | windows.DELETE
		}
		if bits&1 != 0 {
			masks[i] |= windows.FILE_GENERIC_EXECUTE
		}
	}
	var acl strings.Builder
	acl.WriteString("D:P")
	for i, sid := range []string{owner.String(), group.String(), "WD"} {
		// A member of the owner or group class must not gain bits from a
		// later, more permissive class (for example, mode 0646).
		var later uint32
		for _, mask := range masks[i+1:] {
			later |= mask
		}
		if deny := later &^ masks[i]; deny != 0 {
			fmt.Fprintf(&acl, "(D;;0x%x;;;%s)", deny, sid)
		}
		if masks[i] != 0 {
			fmt.Fprintf(&acl, "(A;;0x%x;;;%s)", masks[i], sid)
		}
	}
	return windows.SecurityDescriptorFromString(acl.String())
}

func setMetadata(src, dst *os.File, mode uint32, dir bool) error {
	h := windows.Handle(dst.Fd())
	const principals = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION
	target, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, principals)
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var sd *windows.SECURITY_DESCRIPTOR
	if src == nil {
		sd, err = modeDescriptor(target, mode)
		if err != nil {
			return err
		}
	} else {
		// Including the mandatory label retains integrity restrictions when
		// copying between directories with different inheritance settings.
		sd, err = windows.GetSecurityInfo(windows.Handle(src.Fd()), windows.SE_FILE_OBJECT,
			principals|windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		group, _, err := sd.Group()
		if err != nil {
			return err
		}
		targetOwner, _, err := target.Owner()
		if err != nil {
			return err
		}
		targetGroup, _, err := target.Group()
		if err != nil {
			return err
		}
		if !owner.Equals(targetOwner) {
			info |= windows.OWNER_SECURITY_INFORMATION
		}
		if !group.Equals(targetGroup) {
			info |= windows.GROUP_SECURITY_INFORMATION
		}
		label, _, err := sd.SACL()
		if err != nil && err != windows.ERROR_OBJECT_NOT_FOUND {
			return err
		}
		if label != nil {
			info |= windows.LABEL_SECURITY_INFORMATION
		}
		if dir {
			dacl, _, err := sd.DACL()
			if err != nil {
				return err
			}
			if err = addDirectoryTraverse(dacl); err != nil {
				return err
			}
		}
	}
	// Set by handle without recursively propagating a directory DACL to the
	// already staged children, each of which has its own copy of the leaf ACL.
	if err = windows.SetKernelObjectSecurity(h, info, sd); err != nil {
		return err
	}
	if !dir && mode&0222 == 0 {
		var basic struct {
			Times      [4]int64
			Attributes uint32
			Padding    uint32
		}
		basic.Attributes = windows.FILE_ATTRIBUTE_READONLY
		return windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)))
	}
	return nil
}

// The ACL and ACE layouts are defined in winnt.h. The access mask follows
// the four-byte ACE_HEADER for simple, object and callback access ACEs.
// Carry both read grants and read denials over to traversal. Keep each ACE's
// SID, conditions and inheritance flags.
func addDirectoryTraverse(acl *windows.ACL) error {
	if acl == nil {
		return nil
	}
	h := unsafe.Slice((*byte)(unsafe.Pointer(acl)), 8)
	size, count := int(binary.LittleEndian.Uint16(h[2:])), int(binary.LittleEndian.Uint16(h[4:]))
	if size < 8 {
		return ErrFormat
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(acl)), size)[8:]
	for i := 0; i < count; i++ {
		if len(data) < 8 {
			return ErrFormat
		}
		n := int(binary.LittleEndian.Uint16(data[2:]))
		if n < 8 || n > len(data) {
			return ErrFormat
		}
		switch data[0] {
		case 0, 1, 5, 6, 9, 10, 11, 12: // allow/deny, object and callback variants
			rights := binary.LittleEndian.Uint32(data[4:])
			if rights&windows.FILE_READ_DATA != 0 {
				binary.LittleEndian.PutUint32(data[4:], rights|windows.FILE_TRAVERSE)
			}
		}
		data = data[n:]
	}
	return nil
}
