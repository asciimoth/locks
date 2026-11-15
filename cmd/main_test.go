package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asciimoth/locks"
)

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), args, &out, &diagnostic)
	return code, out.String(), diagnostic.String()
}

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCommands(t *testing.T) {
	root := filepath.Join(tempDir(t), "new", "locks")
	other := filepath.Join(tempDir(t), "locks")
	t.Setenv("LOCK_PATH", other)
	cases := []struct {
		args               []string
		code               int
		output, diagnostic string
	}{
		{[]string{"write", "key", "value", "-v"}, 0, "created\n", "write mode=create"},
		{[]string{"write", "key", "value"}, 0, "same\n", ""},
		{[]string{"write", "key", "different"}, 1, "", "conflict"},
		{[]string{"read", "key", "--verbose"}, 0, "value\n", "found"},
		{[]string{"check", "key"}, 0, "ok\n", ""},
		{[]string{"check", "key", "wrong"}, 1, "", "differs"},
		{[]string{"check", "key", "value\r"}, 0, "ok\n", ""},
		{[]string{"check", "key", "bad\n"}, 2, "", "locks:"},
		{[]string{"write", "--replace", "key", ""}, 0, "replaced\n", ""},
		{[]string{"read", "key"}, 0, "\n", ""},
		{[]string{"read", "missing"}, 1, "", "missing"},
		{[]string{"write", "bad", "bad\nvalue"}, 2, "", "locks:"},
		{[]string{"read", "--id", "bad"}, 2, "", "locks:"},
		{[]string{"write", "--algorithm", "sha256", "key", "sha"}, 0, "created\n", ""},
		{[]string{"read", "--algorithm", "sha256", "key"}, 0, "sha\n", ""},
		{[]string{"doctor", "-v"}, 0, "records=2", "inspect"},
		{[]string{"fix", "-v"}, 0, "overfull=0 pending=0", "recover and split"},
	}
	for _, tc := range cases {
		code, out, diagnostic := invoke(t, append([]string{"--path", root}, tc.args...)...)
		if code != tc.code || !strings.Contains(out, tc.output) || !strings.Contains(diagnostic, tc.diagnostic) {
			t.Fatalf("%v: code=%d out=%q err=%q", tc.args, code, out, diagnostic)
		}
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit path used environment: %v", err)
	}
	id, _ := locks.Hash([]byte("key"), "sha256")
	if code, out, _ := invoke(t, "read", "--path", root, "--id", string(id)); code != 0 || out != "sha\n" {
		t.Fatalf("prehashed read: %d %q", code, out)
	}
	if code, _, _ := invoke(t, "--path", root, "write", "--", "-key", "-value"); code != 0 {
		t.Fatal("literal flags")
	}
	if code, _, _ := invoke(t, "--path", root, "read", strings.Repeat("k", (1<<20)+1)); code != 2 {
		t.Fatal("key limit")
	}
}

func TestParseErrors(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"read"}, {"write", "key"}, {"doctor", "extra"}, {"--unknown"},
		{"--path"}, {"read", "key", "--timeout=bad"}, {"read", "key", "--timeout=0"},
		{"read", "key", "--replace"}, {"write", "key", "value", "--verify"},
		{"doctor", "--id"}, {"read", "key", "--id", "--algorithm=sha256"},
		{"read", "key", "--algorithm=unknown"}, {"--path", "", "read", "key"},
		{"--project", "bad\x00", "read", "key"},
	} {
		if code, _, diagnostic := invoke(t, args...); code != 2 || diagnostic == "" {
			t.Errorf("%q: %d %q", args, code, diagnostic)
		}
	}
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"read", "-h"}} {
		if code, out, _ := invoke(t, args...); code != 0 || !strings.Contains(out, "Usage:") {
			t.Errorf("help: %v", args)
		}
	}
}

func TestEnvironmentAndChain(t *testing.T) {
	dir := tempDir(t)
	one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
	t.Setenv("LOCK_PATH", one+";"+two)
	if code, _, e := invoke(t, "write", "key", "value", "--all", "-v"); code != 0 {
		t.Fatal(e)
	}
	if code, out, _ := invoke(t, "read", "key", "--verify"); code != 0 || out != "value\n" {
		t.Fatal(code, out)
	}
	if code, _, _ := invoke(t, "--path", two, "write", "key", "other", "--replace"); code != 0 {
		t.Fatal(code)
	}
	if code, _, _ := invoke(t, "check", "key"); code != 1 {
		t.Fatal(code)
	}
	t.Setenv("LOCK_PATH", "check,ro="+two+";"+one)
	if code, out, e := invoke(t, "read", "key", "-v"); code != 0 || out != "value\n" || !strings.Contains(e, "skip") {
		t.Fatal(code, out, e)
	}
	t.Setenv("LOCK_PATH", "ro="+two+";"+one)
	if code, _, e := invoke(t, "write", "key", "value", "-v"); code != 0 || !strings.Contains(e, "skip") {
		t.Fatal(code, e)
	}
	if code, _, e := invoke(t, "fix"); code != 0 || !strings.Contains(e, "skip") {
		t.Fatal(code, e)
	}
	if err := os.WriteFile(two, []byte("BAD\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCK_PATH", two+";"+one)
	if code, out, e := invoke(t, "read", "key"); code != 0 || out != "value\n" || !strings.Contains(e, fmt.Sprintf("%q", two)) {
		t.Fatal(code, out, e)
	}
	if code, _, _ := invoke(t, "check", "key"); code != 2 {
		t.Fatal(code)
	}
	if code, _, e := invoke(t, "write", "new", "v", "--all"); code != 0 || !strings.Contains(e, fmt.Sprintf("%q", two)) {
		t.Fatal(code, e)
	}
	if code, _, _ := invoke(t, "doctor"); code != 2 {
		t.Fatal(code)
	}
	t.Setenv("LOCK_PATH", "")
	for _, command := range [][]string{{"read", "key"}, {"write", "key", "v"}, {"doctor"}, {"fix"}, {"git"}} {
		if code, _, _ := invoke(t, command...); code != 2 {
			t.Fatal(command, code)
		}
	}
	t.Setenv("LOCK_PATH", "relative")
	if code, _, _ := invoke(t, "read", "key"); code != 2 {
		t.Fatal(code)
	}
}

func TestHTTP(t *testing.T) {
	handler, err := locks.NewHandler(&locks.Memory{}, locks.HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	if code, _, e := invoke(t, "--path", server.URL, "write", "key", "remote"); code != 0 {
		t.Fatal(e)
	}
	if code, out, e := invoke(t, "--path", server.URL, "read", "key"); code != 0 || out != "remote\n" {
		t.Fatal(code, out, e)
	}
	for _, command := range []string{"doctor", "fix", "git"} {
		if code, _, _ := invoke(t, "--path", server.URL, command); code != 2 {
			t.Fatal(command, code)
		}
	}
	t.Setenv("LOCK_PATH", server.URL+";"+filepath.Join(tempDir(t), "locks"))
	if code, _, _ := invoke(t, "doctor"); code != 0 {
		t.Fatal(code)
	}
}

func TestDoctorRecovery(t *testing.T) {
	dir := tempDir(t)
	path := filepath.Join(dir, "locks")
	id, _ := locks.Hash([]byte("key"), "")
	if err := os.WriteFile(filepath.Join(dir, ".locks.old"), []byte(string(id)+" old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".locks.tmp"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := invoke(t, "--path", path, "doctor"); code != 1 || !strings.Contains(out, "pending=2") {
		t.Fatal(code, out)
	}
	if code, out, e := invoke(t, "--path", path, "fix"); code != 0 || !strings.Contains(out, "pending=0") {
		t.Fatal(code, out, e)
	}
	if code, out, _ := invoke(t, "--path", path, "read", "key"); code != 0 || out != "old\n" {
		t.Fatal(code, out)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestOutputErrorsAndCancellation(t *testing.T) {
	root := filepath.Join(tempDir(t), "locks")
	for _, args := range [][]string{{"write", "key", "v"}, {"read", "key"}, {"check", "key"}, {"doctor"}} {
		code := run(context.Background(), append([]string{"--path", root}, args...), brokenWriter{}, io.Discard)
		if code != 2 {
			t.Fatal(args, code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx, []string{"--path", root, "write", "k", "v"}, io.Discard, io.Discard); code != 2 {
		t.Fatal(code)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := tempDir(t)
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	return dir
}

func TestDiscovery(t *testing.T) {
	dir := initRepo(t)
	t.Setenv("LOCK_PATH", "")
	if err := os.Unsetenv("LOCK_PATH"); err != nil {
		t.Fatal(err)
	}
	a := &app{out: io.Discard, errOut: io.Discard, options: options{project: dir}}
	client, err := a.client(context.Background())
	if err != nil || client.Locations()[0].Address != filepath.Join(dir, ".lock", "locks") {
		t.Fatal(client, err)
	}
}

func TestFixMerge(t *testing.T) {
	root := filepath.Join(tempDir(t), "locks")
	id, _ := locks.Hash([]byte("key"), "")
	var data strings.Builder
	fmt.Fprintf(&data, "%s value\n", id)
	for i := 0; i < 512; i++ {
		other, _ := locks.Hash([]byte(fmt.Sprint(i)), "")
		fmt.Fprintf(&data, "%s other\n", other)
	}
	if err := os.WriteFile(root, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := invoke(t, "--path", root, "fix"); code != 0 {
		t.Fatal(e)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.Name() == string(id)[:2] {
			if err := os.WriteFile(path, []byte(string(id)+" value\n"), 0600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, e := invoke(t, "--path", root, "fix"); code != 0 {
		t.Fatal(e)
	}
	st, _ := os.Stat(root)
	if !st.IsDir() {
		t.Fatal("merged without flag")
	}
	if code, out, e := invoke(t, "--path", root, "fix", "--merge", "-v"); code != 0 || !strings.Contains(out, "leaves=1 records=1") || !strings.Contains(e, "merge small subtrees") {
		t.Fatal(code, out, e)
	}
	st, _ = os.Stat(root)
	if !st.Mode().IsRegular() {
		t.Fatal("did not merge")
	}
	if code, out, e := invoke(t, "--path", root, "read", "key"); code != 0 || out != "value\n" {
		t.Fatal(code, out, e)
	}
	for _, command := range [][]string{{"doctor"}, {"read", "key"}, {"write", "key", "v"}, {"check", "key"}, {"git"}} {
		if code, _, _ := invoke(t, append(command, "--merge")...); code != 2 {
			t.Fatal(command, code)
		}
	}
}
