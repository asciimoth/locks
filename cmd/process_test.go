package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Run the real entry point in a child so os.Exit cannot stop the test process.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("LOCKS_CLI_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"locks"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing argument separator")
}

func childCLI(t *testing.T, dir, path string, args ...string) (int, string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LOCKS_CLI_TEST_PROCESS=1", "LOCK_PATH="+path)
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	err = cmd.Run()
	if err != nil {
		if failure, ok := err.(*exec.ExitError); ok {
			return failure.ExitCode(), out.String(), diagnostic.String()
		}
		t.Fatal(err)
	}
	return 0, out.String(), diagnostic.String()
}

func TestProcessExitAndDiscovery(t *testing.T) {
	dir := initRepo(t)
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "locks")
	for _, tc := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"write", "key", "a value"}, 0, "created\n"},
		{[]string{"read", "key"}, 0, "a value\n"},
		{[]string{"write", "key", "conflict"}, 1, ""},
		{[]string{"read", "missing"}, 1, ""},
		{[]string{"write"}, 2, ""},
	} {
		code, out, diagnostic := childCLI(t, nested, root, tc.args...)
		if code != tc.code || out != tc.out || (code == 0 && diagnostic != "") || (code != 0 && diagnostic == "") {
			t.Fatalf("%q: %d %q %q", tc.args, code, out, diagnostic)
		}
	}
	for _, tc := range []struct {
		cwd, project string
		args         []string
	}{
		{nested, dir, nil},
		{nested, nested, []string{"--project", "."}},
	} {
		args := append([]string{"read", "key", "--verbose"}, tc.args...)
		code, out, diagnostic := childCLI(t, tc.cwd, root, args...)
		if code != 0 || out != "a value\n" || !strings.Contains(diagnostic, "discover stores for project "+strconv.Quote(tc.project)) {
			t.Fatal(code, out, diagnostic)
		}
	}
	outside := tempDir(t)
	code, _, diagnostic := childCLI(t, outside, root, "read", "key", "-v")
	if code != 0 || !strings.Contains(diagnostic, "discover stores for project "+strconv.Quote(outside)) {
		t.Fatal(code, diagnostic)
	}
	code, out, diagnostic := childCLI(t, nested, "", "read", "key")
	if code != 2 || out != "" || diagnostic == "" {
		t.Fatal("empty environment used defaults", code, out, diagnostic)
	}
	code, out, diagnostic = childCLI(t, nested, root, "--help")
	if code != 0 || out != usage || diagnostic != "" {
		t.Fatal(code, out, diagnostic)
	}
}

func TestJustRunArguments(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just unavailable")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tempDir(t), "store with spaces")
	for _, args := range [][]string{
		{"write", "--", "-key with spaces", "value with spaces"},
		{"read", "--", "-key with spaces"},
	} {
		cmd := exec.Command("just", append([]string{"run", "--path", root}, args...)...)
		cmd.Dir = filepath.Dir(dir)
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		if err := cmd.Run(); err != nil {
			t.Fatal(err, diagnostic.String())
		}
		want := "created\n"
		if args[0] == "read" {
			want = "value with spaces\n"
		}
		if out.String() != want {
			t.Fatalf("arguments changed: stdout=%q stderr=%q", out.String(), diagnostic.String())
		}
	}
}
