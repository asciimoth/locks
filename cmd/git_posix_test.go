//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitRootPreservesPathBytes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	for _, name := range []string{"repo\r", "repo\n", "repo\r\n", "repo "} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(tempDir(t), name)
			if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
				t.Fatalf("git init: %s: %v", out, err)
			}
			got, err := gitRoot(context.Background(), root)
			if err != nil || got != root {
				t.Fatalf("gitRoot = %q, %v; want %q", got, err, root)
			}
			if code, out, diagnostic := invoke(t, "git", "--path", filepath.Join(root, "locks")); code != 0 {
				t.Fatal(code, out, diagnostic)
			}
			for _, name := range []string{".gitignore", ".gitattributes"} {
				if _, err := os.Stat(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAddRulesPermissionFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can bypass file permissions")
	}
	for _, tc := range []struct {
		name              string
		dirMode, fileMode os.FileMode
	}{
		{"cannot-stat", 0, 0600},
		{"cannot-read", 0700, 0},
		{"cannot-create-temp", 0500, 0600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			path := filepath.Join(dir, "rules")
			original := []byte("# preserve\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.fileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.dirMode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0700); os.Chmod(path, 0600) })
			if changed, err := addRules(path, []string{"new"}); err == nil || changed {
				t.Fatal(changed, err)
			}
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != string(original) {
				t.Fatal(string(data), err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal("temporary files remain", entries, err)
			}
		})
	}
}
