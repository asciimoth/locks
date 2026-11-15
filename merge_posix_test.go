//go:build darwin || linux || openbsd

package locks

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeRejectsDifferentPermissions(t *testing.T) {
	for _, directory := range []bool{false, true} {
		s, path := healthStore(t)
		records := healthRecords(t, 2)
		seedMergeTree(t, s, records, 1)
		changed := path
		mode := os.FileMode(0750)
		if !directory {
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			changed = filepath.Join(path, entries[len(entries)-1].Name())
			mode = 0640
		}
		if err := os.Chmod(changed, mode); err != nil {
			t.Fatal(err)
		}
		if err := mergeRepair(s); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() {
			t.Fatal(st, err)
		}
	}
}
