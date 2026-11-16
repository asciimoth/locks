//go:build darwin || linux || openbsd

package locks

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNoParallelOmitsFileLocks(t *testing.T) {
	original := system
	t.Cleanup(func() { system = original })
	system.FcntlFlock = func(uintptr, int, *unix.Flock_t) error {
		t.Error("NoParallel called fcntl to lock or unlock a file")
		return unix.ENOSYS
	}
	testLocalNoParallel(t)
}

func TestParentCleanupErrors(t *testing.T) {
	for _, scenario := range []string{"open", "stat", "close"} {
		t.Run(scenario, func(t *testing.T) {
			original := system
			t.Cleanup(func() { system = original })
			primary := errors.New("primary failure")
			cleanup := errors.New("cleanup failure")
			calls := 0
			system.Close = func(fd int) error {
				calls++
				err := original.Close(fd)
				if scenario == "close" && calls == 1 {
					return errors.Join(err, primary)
				}
				return errors.Join(err, cleanup)
			}
			path := "/"
			switch scenario {
			case "open":
				path = "/missing"
				system.Openat = func(int, string, int, uint32) (int, error) { return -1, primary }
			case "stat":
				system.Fstat = func(int, *unix.Stat_t) error { return primary }
			case "close":
				path = "/."
			}
			d, err := openParent(context.Background(), path)
			if d != nil {
				t.Fatal("failed open returned a directory")
			}
			requireErr(t, err, primary)
			requireErr(t, err, cleanup)
		})
	}
}
