//go:build darwin

package locks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinMetadataAcrossSplit(t *testing.T) {
	s := newLocal(t)
	var data strings.Builder
	for i := 0; i < 513; i++ {
		fmt.Fprintf(&data, "%s old\n", testID(i))
	}
	writeFile(t, s.path, data.String())
	if out, err := exec.Command("/bin/chmod", "+a", "everyone allow read", s.path).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	const key = "org.example.locks-test"
	if err := unix.Setxattr(s.path, key, []byte("metadata"), 0); err != nil {
		t.Fatal(err)
	}
	putWant(t, s, testID(0), "new", Replace, Replaced)
	if err := s.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.path, s.path + "/" + string(testID(0))[:2]} {
		got := make([]byte, 32)
		n, err := unix.Getxattr(path, key, got)
		if err != nil || string(got[:n]) != "metadata" {
			t.Fatal(path, n, err)
		}
		out, err := exec.Command("/bin/ls", "-lde", path).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "everyone allow") {
			t.Fatal(string(out), err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() && !strings.Contains(string(out), "search") {
			t.Fatalf("read ACE lacks search on split directory: %s", out)
		}
	}
}
