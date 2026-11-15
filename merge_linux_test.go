package locks

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMergeMetadata(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		s, path := healthStore(t)
		seedMergeTree(t, s, healthRecords(t, 2), 1)
		acl := make([]byte, 4+5*8)
		binary.LittleEndian.PutUint32(acl, 2)
		for i, entry := range []struct {
			tag, perm uint16
			id        uint32
		}{
			{1, 6, ^uint32(0)}, {2, 4, uint32(os.Getuid() + 1)}, {4, 0, ^uint32(0)}, {16, 4, ^uint32(0)}, {32, 0, ^uint32(0)},
		} {
			at := 4 + i*8
			binary.LittleEndian.PutUint16(acl[at:], entry.tag)
			binary.LittleEndian.PutUint16(acl[at+2:], entry.perm)
			binary.LittleEndian.PutUint32(acl[at+4:], entry.id)
		}
		if err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err = unix.Setxattr(path, "user.merge-test", []byte("kept"), 0); err != nil {
				return err
			}
			value := bytes.Clone(acl)
			prepareXattr("system.posix_acl_access", value, info.IsDir())
			return unix.Setxattr(path, "system.posix_acl_access", value, 0)
		}); err != nil {
			t.Skip("ACLs unavailable", err)
		}
		if mismatch {
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(filepath.Join(path, entries[1].Name()), "user.merge-test", []byte("different"), 0); err != nil {
				t.Fatal(err)
			}
			if err := mergeRepair(s); !errors.Is(err, ErrDenied) {
				t.Fatal(err)
			}
			continue
		}
		if err := mergeRepair(s); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		n, err := unix.Getxattr(path, "user.merge-test", buf)
		if err != nil || string(buf[:n]) != "kept" {
			t.Fatal(n, err)
		}
		n, err = unix.Getxattr(path, "system.posix_acl_access", buf)
		if err != nil || !bytes.Equal(buf[:n], acl) {
			t.Fatal(n, err)
		}
	}
}
