//go:build darwin || linux || openbsd

package locks

import (
	"os"

	"golang.org/x/sys/unix"
)

func mergeMetadata(f *os.File, directory bool) (mergeAccess, error) {
	var st unix.Stat_t
	if err := system.Fstat(int(f.Fd()), &st); err != nil {
		return mergeAccess{}, err
	}
	mode := uint32(st.Mode) & 0777
	if directory {
		mode |= (mode & 0444) >> 2
	}
	out := mergeAccess{Mode: mode, Owner: uint64(st.Uid), Group: uint64(st.Gid)}
	return mergeExtraMetadata(f, directory, out)
}
