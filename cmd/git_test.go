package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestGitRules(t *testing.T) {
	ignore, attributes, err := gitRules(".lock/locks")
	if err != nil || !reflect.DeepEqual(ignore, []string{"/.lock/.locks.guard", "/.lock/.locks.tmp", "/.lock/.locks.old", "/.lock/locks/**/.*.tmp", "/.lock/locks/**/.*.old"}) || !reflect.DeepEqual(attributes, []string{".lock/locks text eol=lf", ".lock/locks/** text eol=lf"}) {
		t.Fatal(ignore, attributes, err)
	}
	for _, bad := range []string{"x\ny", "x\ry", "x\x00y"} {
		if _, _, err := gitRules(bad); err == nil {
			t.Fatal(bad)
		}
	}
}

func TestGitIntegration(t *testing.T) {
	dir := initRepo(t)
	ignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("# keep this\r\nother"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, ".lock", "locks")
	for i := 0; i < 2; i++ {
		code, out, diagnostic := invoke(t, "git", "--path", root, "-v")
		want := "updated"
		if i > 0 {
			want = "unchanged"
		}
		if code != 0 || strings.Count(out, want) != 2 || !strings.Contains(diagnostic, "Git rules") {
			t.Fatal(code, out, diagnostic)
		}
	}
	data, err := os.ReadFile(ignorePath)
	if err != nil || !strings.HasPrefix(string(data), "# keep this\r\nother\n") || strings.Count(string(data), "/.lock/.locks.guard") != 1 {
		t.Fatal(string(data), err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(ignorePath)
		if st.Mode().Perm() != 0600 {
			t.Fatal(st.Mode())
		}
	}
	for _, name := range []string{".lock/.locks.guard", ".lock/.locks.old", ".lock/.locks.tmp", ".lock/locks/AB/.C.tmp", ".lock/locks/AB/.C.old"} {
		if out, err := exec.Command("git", "-C", dir, "check-ignore", "--no-index", name).CombinedOutput(); err != nil {
			t.Fatalf("%s: %s %v", name, out, err)
		}
	}
	for _, name := range []string{".lock/locks", ".lock/locks/AB", "unrelated/.x.tmp", ".lock/.other.guard"} {
		if err := exec.Command("git", "-C", dir, "check-ignore", "--no-index", name).Run(); err == nil {
			t.Fatal("unexpected ignore", name)
		}
	}
	for _, name := range []string{".lock/locks", ".lock/locks/AB/C"} {
		out, err := exec.Command("git", "-C", dir, "check-attr", "text", "eol", "--", name).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "text: set") || !strings.Contains(string(out), "eol: lf") {
			t.Fatal(string(out), err)
		}
	}
	if code := run(context.Background(), []string{"git", "--path", root}, brokenWriter{}, io.Discard); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := invoke(t, "git", "--path", dir); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := invoke(t, "git", "--path", filepath.Join(tempDir(t), "locks")); code != 2 {
		t.Fatal(code)
	}
	t.Setenv("LOCK_PATH", root+";"+filepath.Join(tempDir(t), "locks")+";https://example.invalid")
	if code, _, _ := invoke(t, "git", "-v"); code != 0 {
		t.Fatal(code)
	}
}

func TestGitLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("glob characters are not valid Windows paths")
	}
	dir := initRepo(t)
	for _, name := range []string{"space dir/lock name", "glob[ab]/lock*?", "plain", "#hash", "!bang", "tab\tname", `quote"name`, `back\slash`} {
		root := filepath.Join(dir, filepath.FromSlash(name))
		if code, _, e := invoke(t, "git", "--path", root); code != 0 {
			t.Fatal(e)
		}
		guard := filepath.ToSlash(filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+".guard"))
		if out, err := exec.Command("git", "-C", dir, "check-ignore", "--no-index", guard).CombinedOutput(); err != nil {
			t.Fatal(guard, string(out), err)
		}
		out, err := exec.Command("git", "-C", dir, "check-attr", "eol", "--", name, name+"/AB").CombinedOutput()
		if err != nil || strings.Count(string(out), "eol: lf") != 2 {
			t.Fatal(name, string(out), err)
		}
	}
}

func TestAddRulesFailures(t *testing.T) {
	dir := tempDir(t)
	if _, err := addRules(dir, []string{"x"}); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := addRules(filepath.Join(dir, "missing", "file"), []string{"x"}); err == nil {
		t.Fatal("missing parent accepted")
	}
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("x\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := addRules(path, []string{"x", "x"}); err != nil || changed {
		t.Fatal(changed, err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := addRules(link, []string{"y"}); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}

func TestGitFileFailuresContinue(t *testing.T) {
	for _, blocked := range []string{".gitignore", ".gitattributes"} {
		t.Run(blocked, func(t *testing.T) {
			dir := initRepo(t)
			if err := os.Mkdir(filepath.Join(dir, blocked), 0700); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dir, "new", "deep", "locks")
			code, out, diagnostic := invoke(t, "git", "--path", root)
			if code != 2 || strings.Count(out, "updated") != 1 || !strings.Contains(diagnostic, "not a regular file") {
				t.Fatal(code, out, diagnostic)
			}
			other := ".gitignore"
			if blocked == other {
				other = ".gitattributes"
			}
			if data, err := os.ReadFile(filepath.Join(dir, other)); err != nil || len(data) == 0 {
				t.Fatal(string(data), err)
			}
			if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
				t.Fatal("Git command created store parents", err)
			}
			files, err := filepath.Glob(filepath.Join(dir, ".locks-git-*"))
			if err != nil || len(files) != 0 {
				t.Fatal("temporary file left behind", files, err)
			}
		})
	}
}

func TestGitRejectsUnrepresentablePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("line breaks are not valid Windows paths")
	}
	dir := initRepo(t)
	for _, name := range []string{"bad\nname", "bad\rname"} {
		code, out, diagnostic := invoke(t, "git", "--path", filepath.Join(dir, name))
		if code != 2 || out != "" || !strings.Contains(diagnostic, "line breaks") {
			t.Fatal(code, out, diagnostic)
		}
	}
	for _, name := range []string{".gitignore", ".gitattributes"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("invalid path changed Git files", err)
		}
	}
}

func TestAddRulesPreservesContent(t *testing.T) {
	for _, initial := range []string{"", "# comment", "# comment\n", "# comment\r\nx\r\n"} {
		t.Run(fmt.Sprintf("%q", initial), func(t *testing.T) {
			path := filepath.Join(tempDir(t), "rules")
			if err := os.WriteFile(path, []byte(initial), 0640); err != nil {
				t.Fatal(err)
			}
			changed, err := addRules(path, []string{"x", "y", "x", "y"})
			if err != nil || !changed {
				t.Fatal(changed, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := initial
			if len(want) > 0 && !strings.HasSuffix(want, "\n") {
				want += "\n"
			}
			if !strings.Contains(initial, "x\r\n") {
				want += "x\n"
			}
			want += "y\n"
			if string(data) != want {
				t.Fatalf("got %q want %q", data, want)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := addRules(path, []string{"y", "x"}); err != nil || changed {
				t.Fatal(changed, err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("unchanged rules replaced the file", err)
			}
		})
	}
	path := filepath.Join(tempDir(t), "absent")
	if changed, err := addRules(path, nil); err != nil || changed {
		t.Fatal(changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
