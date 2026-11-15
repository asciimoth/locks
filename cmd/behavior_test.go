package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asciimoth/locks"
)

func requireCommand(t *testing.T, code int, output string, args ...string) string {
	t.Helper()
	got, out, diagnostic := invoke(t, args...)
	if got != code || out != output {
		t.Fatalf("%q: code=%d stdout=%q stderr=%q; want code=%d stdout=%q", args, got, out, diagnostic, code, output)
	}
	return diagnostic
}

func TestParseOptionPlacement(t *testing.T) {
	for _, args := range [][]string{
		{"--path", "one", "--path=two", "-v", "write", "key", "value", "--all", "--replace"},
		{"write", "--path=one", "key", "--all", "--path", "two", "value", "--replace", "--verbose"},
	} {
		a := &app{}
		command, operands, err := a.parse(args)
		if err != nil || command != "write" || !reflect.DeepEqual(operands, []string{"key", "value"}) || !reflect.DeepEqual(a.options.paths, paths{"one", "two"}) || !a.options.verbose || !a.options.all || !a.options.replace {
			t.Fatalf("%q: %s %q %+v %v", args, command, operands, a.options, err)
		}
	}
	for _, args := range [][]string{
		{"write", "--", "--path", "-v"},
		{"write", "--", "-", ""},
		{"write", "--", "", "--"},
	} {
		a := &app{}
		command, operands, err := a.parse(args)
		if err != nil || command != "write" || !reflect.DeepEqual(operands, args[2:]) || a.options.verbose || len(a.options.paths) != 0 {
			t.Fatalf("literal operands: %q %q %v", args, operands, err)
		}
	}
	for _, args := range [][]string{{"read", "key", "--verbose=wrong"}, {"read", "key", "--timeout=-1s"}, {"fix", "--all=false"}, {"doctor", "--merge=false"}, {"check", "key", "--verify=false"}, {"git", "--algorithm=blake2"}} {
		if code, out, diagnostic := invoke(t, args...); code != 2 || out != "" || diagnostic == "" {
			t.Fatalf("%q: %d %q %q", args, code, out, diagnostic)
		}
	}
}

func TestChainPolicies(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%t", all), func(t *testing.T) {
			one, two := filepath.Join(tempDir(t), "one"), filepath.Join(tempDir(t), "two")
			args := []string{"--path", one, "--path", two, "write", "key", "secret", "--verbose"}
			if all {
				args = append(args, "--all")
			}
			diagnostic := requireCommand(t, 0, "created\n", args...)
			if strings.Contains(diagnostic, "secret") {
				t.Fatal("value leaked to logs", diagnostic)
			}
			requireCommand(t, 0, "secret\n", "--path", one, "read", "key")
			if all {
				requireCommand(t, 0, "secret\n", "--path", two, "read", "key")
			} else {
				requireCommand(t, 1, "", "--path", two, "read", "key")
				if !strings.Contains(diagnostic, "did not visit") {
					t.Fatal(diagnostic)
				}
			}
			status := "created\n"
			if all {
				status = "replaced\n"
			}
			requireCommand(t, 0, status, "--path", two, "write", "key", "other", "--replace")
		})
	}
}

func TestWitnessAndOrderedReads(t *testing.T) {
	one, two, missing := filepath.Join(tempDir(t), "one"), filepath.Join(tempDir(t), "two"), filepath.Join(tempDir(t), "missing")
	requireCommand(t, 0, "created\n", "--path", one, "write", "key", "first")
	requireCommand(t, 0, "created\n", "--path", two, "write", "key", "second")
	requireCommand(t, 0, "first\n", "--path", one, "--path", two, "read", "key")
	requireCommand(t, 0, "second\n", "--path", two, "--path", one, "read", "key")
	requireCommand(t, 1, "", "--path", one, "--path", two, "read", "key", "--verify")
	requireCommand(t, 0, "ok\n", "--path", missing, "--path", one, "check", "key", "first")
	t.Setenv("LOCK_PATH", "check="+two+";"+one)
	requireCommand(t, 0, "first\n", "read", "key")
	requireCommand(t, 1, "", "check", "key")
	t.Setenv("LOCK_PATH", "check="+two)
	requireCommand(t, 2, "", "read", "key", "--verify")
	t.Setenv("LOCK_PATH", "ro="+one+";check="+missing)
	requireCommand(t, 0, "created\n", "write", "new", "witness", "--all")
	requireCommand(t, 1, "", "--path", one, "read", "new")
	requireCommand(t, 0, "witness\n", "--path", missing, "read", "new")
}

func TestLiteralKeyAndValueRoundTrip(t *testing.T) {
	root := filepath.Join(tempDir(t), "locks")
	for i, value := range []string{"", " ", "  spaced\tvalue  ", "snow 雪", "--all", "line\rcarriage", "final\r"} {
		key := fmt.Sprintf("-%d\n雪", i)
		expected, err := locks.NormalizeValue(value)
		if err != nil {
			t.Fatal(err)
		}
		requireCommand(t, 0, "created\n", "--path", root, "write", "--", key, value)
		requireCommand(t, 0, expected+"\n", "--path", root, "read", "--", key)
		requireCommand(t, 0, "ok\n", "--path", root, "check", "--", key, value)
	}
}

func TestHTTPFailureAndDeadline(t *testing.T) {
	root := filepath.Join(tempDir(t), "locks")
	requireCommand(t, 0, "created\n", "--path", root, "write", "key", "local")
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failure", status) }))
			defer server.Close()
			diagnostic := requireCommand(t, 0, "local\n", "--path", server.URL, "--path", root, "read", "key")
			if !strings.Contains(diagnostic, server.URL) {
				t.Fatal(diagnostic)
			}
			requireCommand(t, 2, "", "--path", server.URL, "--path", root, "check", "key")
		})
	}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	for _, args := range [][]string{{"read", "key"}, {"write", "key", "value"}, {"check", "key"}} {
		diagnostic := requireCommand(t, 2, "", append([]string{"--path", server.URL, "--timeout=50ms"}, args...)...)
		if !strings.Contains(diagnostic, "deadline") {
			t.Fatal(diagnostic)
		}
	}
}

func TestMaintenancePreservesDataAndContinues(t *testing.T) {
	dir := tempDir(t)
	bad, good := filepath.Join(dir, "bad"), filepath.Join(dir, "good")
	if err := os.WriteFile(bad, []byte("BAD\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, _ := locks.Hash([]byte("key"), "")
	old := filepath.Join(dir, ".good.old")
	data := []byte(string(id) + " value\n")
	if err := os.WriteFile(old, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{{bad, good}, {good, bad}} {
		code, out, diagnostic := invoke(t, "--path", order[0], "--path", order[1], "doctor")
		if code != 2 || !strings.Contains(out, "pending=1") || !strings.Contains(diagnostic, fmt.Sprintf("%q", bad)) {
			t.Fatal(code, out, diagnostic)
		}
		got, err := os.ReadFile(old)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("doctor changed recovery data", string(got), err)
		}
	}
	code, out, diagnostic := invoke(t, "--path", bad, "--path", good, "fix", "--merge")
	if code != 2 || !strings.Contains(out, "pending=0") || !strings.Contains(diagnostic, fmt.Sprintf("%q", bad)) {
		t.Fatal(code, out, diagnostic)
	}
	requireCommand(t, 0, "value\n", "--path", good, "read", "key")
	got, err := os.ReadFile(bad)
	if err != nil || string(got) != "BAD\n" {
		t.Fatal("repair changed malformed data", string(got), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, command := range []string{"doctor", "fix"} {
		var out, diagnostic bytes.Buffer
		if code := run(ctx, []string{"--path", good, command}, &out, &diagnostic); code != 2 || out.Len() != 0 {
			t.Fatal(command, code, out.String(), diagnostic.String())
		}
	}
}
