//go:build darwin || linux || openbsd

package locks

import (
	"os"
	"testing"
)

func TestLocalPreservesMode(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "v", Create, Created)
	if e := os.Chmod(s.path, 0640); e != nil {
		t.Fatal(e)
	}
	putWant(t, s, abc, "new", Replace, Replaced)
	st, e := os.Stat(s.path)
	if e != nil || st.Mode().Perm() != 0640 {
		t.Fatal(st, e)
	}
}
