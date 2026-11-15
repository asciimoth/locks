//go:build darwin || linux

package locks

import (
	"context"
	"testing"
)

func TestXattrSizeChanges(t *testing.T) {
	for _, scenario := range []string{"negative-list", "large-list", "negative-value", "growing-empty-value", "negative-read"} {
		t.Run(scenario, func(t *testing.T) {
			s := newLocal(t)
			putWant(t, s, abc, "old", Create, Created)
			original := xattrs
			t.Cleanup(func() { xattrs = original })
			listCalls := 0
			xattrs.Flistxattr = func(_ int, b []byte) (int, error) {
				listCalls++
				if scenario == "negative-list" {
					return -1, nil
				}
				if scenario == "large-list" && listCalls == 2 {
					return len(b) + 1, nil
				}
				copy(b, "user.test\x00")
				return len("user.test\x00"), nil
			}
			valueCalls := 0
			xattrs.Fgetxattr = func(_ int, _ string, _ []byte) (int, error) {
				valueCalls++
				if scenario == "negative-value" || scenario == "negative-read" && valueCalls == 2 {
					return -1, nil
				}
				if valueCalls == 1 {
					return 0, nil
				}
				return 1, nil
			}
			xattrs.Fsetxattr = func(int, string, []byte, int) error {
				t.Fatal("invalid attribute was copied")
				return nil
			}
			_, err := s.Put(context.Background(), abc, "new", Replace)
			requireErr(t, err, ErrFormat)
			getWant(t, s, abc, Found, "old")
		})
	}
}
