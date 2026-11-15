package locks

import (
	"encoding/binary"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func mergeACL(f *os.File, directory bool) (string, error) {
	acl, err := readACL(f)
	if errors.Is(err, unix.ENOTSUP) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if directory {
		for at := filesecHeader; at < len(acl); at += aceSize {
			flags := binary.NativeEndian.Uint32(acl[at+16:])
			rights := binary.NativeEndian.Uint32(acl[at+20:])
			if (flags&0xf == 1 || flags&0xf == 2) && rights&(1<<1) != 0 {
				binary.NativeEndian.PutUint32(acl[at+20:], rights|1<<3)
			}
		}
	}
	return string(acl), nil
}
