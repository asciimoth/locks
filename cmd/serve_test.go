package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asciimoth/locks"
)

type serverAddress chan string

func (c serverAddress) Write(p []byte) (int, error) {
	c <- strings.TrimSpace(string(p))
	return len(p), nil
}

func TestServe(t *testing.T) {
	for _, noParallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-parallel=%t", noParallel), func(t *testing.T) {
			testServe(t, noParallel)
		})
	}
}

func testServe(t *testing.T, noParallel bool) {
	root := filepath.Join(tempDir(t), "new", "locks")
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "read-only"}[readOnly], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			address := make(serverAddress, 1)
			done := make(chan int, 1)
			var diagnostic bytes.Buffer
			args := []string{"serve", "--path", root, "--listen", "127.0.0.1:0", "--timeout", "200ms"}
			if noParallel {
				args = append(args, "--no-parallel")
			}
			if readOnly {
				args = append(args, "--read-only")
			}
			go func() { done <- run(ctx, args, address, &diagnostic) }()
			t.Cleanup(func() {
				cancel()
				select {
				case code := <-done:
					if code != 0 {
						t.Errorf("server exit %d: %s", code, diagnostic.String())
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not stop")
				}
			})
			var base string
			select {
			case base = <-address:
			case <-time.After(5 * time.Second):
				t.Fatal("server did not start")
			}
			// The operation timeout must not stop the server.
			time.Sleep(250 * time.Millisecond)
			code, _, detail := invoke(t, "--path", base, "write", "key", "value")
			if readOnly {
				if code != 2 {
					t.Fatalf("read-only write: %d %s", code, detail)
				}
			} else if code != 0 {
				t.Fatalf("write: %d %s", code, detail)
			}
			if code, out, detail := invoke(t, "--path", base, "read", "key"); code != 0 || out != "value\n" {
				t.Fatalf("read: %d %q %s", code, out, detail)
			}
			if !readOnly {
				if code, _, _ := invoke(t, "--path", base, "write", "--create", "key", "different"); code != 1 {
					t.Fatalf("conflict: %d", code)
				}
			}
			client := &http.Client{Timeout: time.Second}
			for _, tc := range []struct {
				path   string
				status int
			}{{"/", 404}, {"/lock/extra", 404}, {"/lock?key=bad", 400}} {
				response, err := client.Get(base + tc.path)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != tc.status {
					t.Errorf("%s: %d", tc.path, response.StatusCode)
				}
			}
		})
	}
	if code, out, detail := invoke(t, "--path", root, "read", "key"); code != 0 || out != "value\n" {
		t.Fatal(code, out, detail)
	}
}

func TestServeErrors(t *testing.T) {
	root := filepath.Join(tempDir(t), "locks")
	for _, args := range [][]string{
		{"serve"}, {"serve", "extra"}, {"serve", "--path", ""},
		{"serve", "--path", root, "--path", root},
		{"serve", "--path", "https://example.com"},
		{"serve", "--path", root, "--project", "."},
		{"serve", "--path", root, "--listen", "invalid"},
		{"serve", "--path", "bad\x00"},
		{"read", "key", "--listen", ":8080"}, {"doctor", "--read-only"},
		{"write", "key", "value", "--append"},
		{"read", "key", "--no-parallel"},
		{"write", "key", "value", "--no-parallel=false"},
		{"doctor", "--no-parallel"}, {"fix", "--no-parallel"},
		{"git", "--no-parallel"},
		{"serve", "--path", root, "--no-parallel=invalid"},
	} {
		if code, _, diagnostic := invoke(t, args...); code != 2 || diagnostic == "" {
			t.Errorf("%q: %d %s", args, code, diagnostic)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if code, _, _ := invoke(t, "serve", "--path", root, "--listen", listener.Addr().String()); code != 2 {
		t.Fatal(code)
	}
	if code := run(context.Background(), []string{"serve", "--path", root, "--listen", "127.0.0.1:0"}, brokenWriter{}, io.Discard); code != 2 {
		t.Fatal(code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx, []string{"serve", "--path", root, "--listen", "127.0.0.1:0"}, io.Discard, io.Discard); code != 0 && code != 2 {
		t.Fatal(code)
	}
}

func TestServeAppend(t *testing.T) {
	for _, noParallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-parallel=%t", noParallel), func(t *testing.T) {
			testServeAppend(t, noParallel)
		})
	}
}

func TestAppendStoreRejectsInvalidMode(t *testing.T) {
	local, err := locks.OpenLocal(filepath.Join(tempDir(t), "locks"), locks.LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := locks.Hash([]byte("key"), "blake2")
	if err != nil {
		t.Fatal(err)
	}
	store := appendStore{local}
	if _, err := store.Put(context.Background(), id, "value", locks.Mode("invalid")); !errors.Is(err, locks.ErrFormat) {
		t.Fatalf("invalid mode: %v", err)
	}
	result, err := store.Get(context.Background(), id)
	if err != nil || result.Status != locks.Missing {
		t.Fatalf("invalid mode wrote a key: %+v, %v", result, err)
	}
}

func testServeAppend(t *testing.T, noParallel bool) {
	root := filepath.Join(tempDir(t), "locks")
	if code, _, detail := invoke(t, "--path", root, "write", "old", "first"); code != 0 {
		t.Fatal(code, detail)
	}
	ctx, cancel := context.WithCancel(context.Background())
	address := make(serverAddress, 1)
	done := make(chan int, 1)
	var diagnostic bytes.Buffer
	var idle net.Conn
	go func() {
		done <- run(ctx, []string{"serve", "--path", root, "--listen", "127.0.0.1:0", "--timeout", "1s", "--append", fmt.Sprintf("--no-parallel=%t", noParallel)}, address, &diagnostic)
	}()
	t.Cleanup(func() {
		if idle != nil {
			defer idle.Close()
		}
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("server exit %d: %s", code, diagnostic.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	var base string
	select {
	case base = <-address:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	oldID, _ := locks.Hash([]byte("old"), "blake2")
	newID, _ := locks.Hash([]byte("new"), "blake2")
	body := "locks/1 put\n" +
		"create raw.blake2.b2xk first\n" +
		"replace raw.blake2.bmV3 second\n"
	request, err := http.NewRequest(http.MethodPost, base+"/lock", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("locks/1 results\nerror %s denied\ncreated %s\n", oldID, newID)
	if response.StatusCode != http.StatusOK || string(data) != want {
		t.Fatalf("batch: %d %q; want %q", response.StatusCode, data, want)
	}
	for _, args := range [][]string{
		{"write", "old", "first"},
		{"write", "old", "changed"},
		{"write", "old", "changed", "--create"},
		{"write", "new", "second"},
	} {
		if code, _, detail := invoke(t, append([]string{"--path", base}, args...)...); code != 2 || !strings.Contains(detail, "denied") {
			t.Fatalf("%v: %d %s", args, code, detail)
		}
	}
	for _, tc := range []struct{ key, value string }{{"old", "first"}, {"new", "second"}} {
		if code, out, detail := invoke(t, "--path", base, "read", tc.key); code != 0 || out != tc.value+"\n" {
			t.Fatalf("%s: %d %q %s", tc.key, code, out, detail)
		}
	}
	codes := make(chan int, 2)
	for _, value := range []string{"one", "two"} {
		go func(value string) {
			code, _, _ := invoke(t, "--path", base, "write", "race", value)
			codes <- code
		}(value)
	}
	first, second := <-codes, <-codes
	if first+second != 2 || first == second {
		t.Fatalf("concurrent writes: %d, %d", first, second)
	}
	if code, out, detail := invoke(t, "--path", base, "read", "race"); code != 0 || out != "one\n" && out != "two\n" {
		t.Fatalf("concurrent value: %d %q %s", code, out, detail)
	}
	// An unused connection must not keep shutdown running past its timeout.
	idle, err = net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
}
