package locks

import (
	"os"

	"golang.org/x/sys/windows"
)

func mergeMetadata(f *os.File, directory bool) (mergeAccess, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return mergeAccess{}, err
	}
	if directory {
		acl, _, err := sd.DACL()
		if err != nil {
			return mergeAccess{}, err
		}
		if err = addDirectoryTraverse(acl); err != nil {
			return mergeAccess{}, err
		}
	}
	st, err := f.Stat()
	if err != nil {
		return mergeAccess{}, err
	}
	mode := uint32(st.Mode().Perm())
	if directory {
		mode |= (mode & 0444) >> 2
	}
	acl := sd.String()
	if acl == "" {
		return mergeAccess{}, ErrFormat
	}
	return mergeAccess{Mode: mode, ACL: acl}, nil
}
