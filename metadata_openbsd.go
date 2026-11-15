//go:build openbsd

package locks

import "os"

// OpenBSD's native access controls are ownership and the Unix permission bits.
func setMetadata(src, dst *os.File, mode uint32, dir bool) error {
	return setOwnerMode(src, dst, mode)
}
