//go:build linux

package locks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLocalConfiguration(t *testing.T) {
	for _, tt := range []struct {
		p   string
		o   LocalOptions
		err error
	}{{"relative", LocalOptions{}, ErrFormat}, {"/", LocalOptions{}, ErrFormat}, {"/tmp/.locks.guard", LocalOptions{}, ErrFormat}, {"/tmp/x", LocalOptions{Limits: Limits{ScanBytes: -1}}, ErrLimit}, {"/tmp/x", LocalOptions{LockTimeout: -1}, ErrLimit}, {"/tmp/x", LocalOptions{FileMode: os.ModeSymlink}, ErrFormat}} {
		_, e := OpenLocal(tt.p, tt.o)
		requireErr(t, e, tt.err)
	}
	s, e := OpenLocal(filepath.Join(t.TempDir(), "missing", "locks"), LocalOptions{})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Get(context.Background(), abc)
	if e == nil {
		t.Fatal("missing parent became miss")
	}
	if e = s.Validate(context.Background()); e == nil {
		t.Fatal("missing parent validated")
	}
	root := t.TempDir()
	if e = os.Symlink(root, root+"-link"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Remove(root + "-link") })
	s, _ = OpenLocal(filepath.Join(root+"-link", "locks"), LocalOptions{})
	_, e = s.Get(context.Background(), abc)
	if e == nil {
		t.Fatal("followed parent symlink")
	}
}
func TestMalformedStoreAndEvidence(t *testing.T) {
	for _, kind := range []string{"duplicate", "misplaced", "marker", "guard-link", "guard-hardlink", "root-link", "root-fifo", "backup-dir", "backup-hardlink", "stage-link", "stage-child-link", "bad-published", "bad-backup", "unknown-child", "lowercase-child", "deep-directory", "sidecars-no-guard"} {
		t.Run(kind, func(t *testing.T) {
			s := newLocal(t)
			guardPath := filepath.Join(s.parent, ".locks.guard")
			old := filepath.Join(s.parent, ".locks.old")
			tmp := filepath.Join(s.parent, ".locks.tmp")
			writeFile(t, guardPath, "")
			writeFile(t, s.path, string(abc)+" v\n")
			switch kind {
			case "duplicate":
				writeFile(t, s.path, string(abc)+" v\n"+string(abc)+" v\n")
			case "marker":
				writeFile(t, s.path, "<<<<<<< HEAD\n")
			case "guard-link":
				_ = os.Remove(guardPath)
				_ = os.Symlink(s.path, guardPath)
			case "guard-hardlink":
				_ = os.Link(guardPath, guardPath+"-link")
			case "root-link":
				_ = os.Remove(s.path)
				_ = os.Symlink(guardPath, s.path)
			case "root-fifo":
				_ = os.Remove(s.path)
				if e := unix.Mkfifo(s.path, 0600); e != nil {
					t.Fatal(e)
				}
			case "backup-dir":
				_ = os.Mkdir(old, 0700)
			case "backup-hardlink":
				writeFile(t, old, string(abc)+" old\n")
				_ = os.Link(old, old+"-link")
			case "stage-link":
				_ = os.Symlink(s.path, tmp)
			case "stage-child-link":
				_ = os.Mkdir(tmp, 0700)
				_ = os.Symlink(s.path, filepath.Join(tmp, "AA"))
			case "bad-published":
				writeFile(t, old, string(abc)+" old\n")
				writeFile(t, s.path, "BAD\n")
			case "bad-backup":
				_ = os.Remove(s.path)
				writeFile(t, old, "BAD\n")
			case "sidecars-no-guard":
				_ = os.Remove(guardPath)
				writeFile(t, tmp, "")
			default:
				_ = os.Remove(s.path)
				_ = os.Mkdir(s.path, 0700)
				switch kind {
				case "misplaced":
					writeFile(t, filepath.Join(s.path, "XX"), string(testID(1))+" v\n")
				case "unknown-child":
					writeFile(t, filepath.Join(s.path, "unknown"), "untouched")
				case "lowercase-child":
					writeFile(t, filepath.Join(s.path, "xx"), "untouched")
				case "deep-directory":
					p := filepath.Join(s.path, "XX")
					for i := 2; i < 52; i++ {
						p = filepath.Join(p, string(abc[i]))
					}
					_ = os.MkdirAll(p, 0700)
				}
			}
			_, readErr := s.Get(context.Background(), abc)
			if kind != "unknown-child" && kind != "lowercase-child" && kind != "stage-child-link" && readErr == nil {
				t.Fatal("read accepted malformed store")
			}
			if kind == "sidecars-no-guard" {
				return
			}
			if e := s.Recover(context.Background()); e == nil {
				t.Fatal("recovery accepted corruption")
			}
			if e := s.Validate(context.Background()); e == nil {
				t.Fatal("validation accepted corruption")
			}
			if kind == "bad-published" {
				if _, e := os.Stat(old); e != nil {
					t.Fatal("lost evidence", e)
				}
			}
		})
	}
}
func TestGuardIdentityAndUnsafeTypes(t *testing.T) {
	s := newLocal(t)
	d, e := openParent(context.Background(), s.parent)
	if e != nil {
		t.Fatal(e)
	}
	defer d.f.Close()
	g, e := getGuard(d, ".locks.guard", true)
	if e != nil {
		t.Fatal(e)
	}
	alias, e := getGuard(d, ".locks.guard", false)
	if e != nil || alias != g {
		t.Fatal(alias, e)
	}
	if e = alias.close(); e != nil {
		t.Fatal(e)
	}
	g.writable = false
	_, e = getGuard(d, ".locks.guard", true)
	requireErr(t, e, ErrDenied)
	g.writable = true
	if e = g.close(); e != nil {
		t.Fatal(e)
	}
	_ = os.Mkdir(s.path, 0700)
	_, e = d.open(s.name, false)
	requireErr(t, e, ErrFormat)
	_, e = d.child("absent")
	if !errors.Is(e, unix.ENOENT) {
		t.Fatal(e)
	}
}

func TestOptimisticReadRetries(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprint(batch, "/", split), func(t *testing.T) {
				s := newLocal(t)
				if split {
					var b strings.Builder
					for i := 0; i < 512; i++ {
						fmt.Fprintf(&b, "%s old\n", testID(i))
					}
					writeFile(t, s.path, b.String())
				}
				original := system
				defer func() { system = original }()
				triggered := false
				if split {
					system.Openat = func(fd int, name string, flags int, mode uint32) (int, error) {
						if name == s.name && !triggered {
							triggered = true
							putWant(t, s, abc, "new", Create, Created)
						}
						return original.Openat(fd, name, flags, mode)
					}
				} else {
					system.Fstatat = func(fd int, name string, st *unix.Stat_t, flags int) error {
						e := original.Fstatat(fd, name, st, flags)
						if name == s.name && !triggered {
							triggered = true
							putWant(t, s, abc, "new", Create, Created)
						}
						return e
					}
				}
				if batch {
					out, e := s.GetBatch(context.Background(), []ID{abc})
					if e != nil || out[0].Err != nil || out[0].Result.Value != "new" {
						t.Fatal(out, e)
					}
				} else {
					getWant(t, s, abc, Found, "new")
				}
				if !triggered {
					t.Fatal("race was not exercised")
				}
			})
		}
	}
}
func TestGuardOpenFallbackAndCreationRace(t *testing.T) {
	for _, reason := range []error{unix.EACCES, unix.EROFS, unix.EEXIST} {
		t.Run(reason.Error(), func(t *testing.T) {
			s := newLocal(t)
			d, e := openParent(context.Background(), s.parent)
			if e != nil {
				t.Fatal(e)
			}
			defer d.f.Close()
			original := system
			defer func() { system = original }()
			injected := false
			if reason != unix.EEXIST {
				writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
			}
			system.Openat = func(fd int, name string, flags int, mode uint32) (int, error) {
				if name == ".locks.guard" && !injected {
					injected = true
					if reason == unix.EEXIST {
						writeFile(t, filepath.Join(s.parent, name), "")
					}
					return -1, reason
				}
				return original.Openat(fd, name, flags, mode)
			}
			g, e := getGuard(d, ".locks.guard", reason == unix.EEXIST)
			if e != nil {
				t.Fatal(e)
			}
			if reason != unix.EEXIST && g.writable {
				t.Fatal("fallback did not use read-only descriptor")
			}
			if e = g.lock(context.Background(), 0, false); e != nil {
				t.Fatal(e)
			}
			if e = g.unlock(0); e != nil {
				t.Fatal(e)
			}
			if e = g.close(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestKilledPublicationRecovery(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, phase := range []string{"stage", "backup", "publish", "cleanup"} {
			t.Run(fmt.Sprint(nested, "/", phase), func(t *testing.T) {
				s := newLocal(t)
				live := s.path
				if nested {
					if e := os.Mkdir(s.path, 0700); e != nil {
						t.Fatal(e)
					}
					live = filepath.Join(s.path, "XX")
				}
				var b strings.Builder
				oldID := ID(string(abc)[:52] + ":old-0")
				for i := 0; i < 512; i++ {
					fmt.Fprintf(&b, "%s:old-%d old\n", string(abc)[:52], i)
				}
				writeFile(t, live, b.String())
				cmd := exec.Command(os.Args[0], "-test.run=^TestPublicationHelper$")
				cmd.Env = append(os.Environ(), "LOCKS_TEST_CHILD=1", "LOCKS_TEST_PATH="+s.path, "LOCKS_TEST_PHASE="+phase, "LOCKS_TEST_VALUE=new")
				stdout, e := cmd.StdoutPipe()
				if e != nil {
					t.Fatal(e)
				}
				if e = cmd.Start(); e != nil {
					t.Fatal(e)
				}
				defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
				ready := make([]byte, 6)
				if _, e = io.ReadFull(stdout, ready); e != nil {
					t.Fatal(e)
				}
				if string(ready) != "READY\n" {
					t.Fatal(string(ready))
				}
				if e = cmd.Process.Kill(); e != nil {
					t.Fatal(e)
				}
				_ = cmd.Wait()
				getWant(t, s, oldID, Found, "old")
				want, value := Missing, ""
				if phase == "publish" || phase == "cleanup" {
					want, value = Found, "new"
				}
				getWant(t, s, abc, want, value)
				if e = s.Recover(context.Background()); e != nil {
					t.Fatal(e)
				}
				getWant(t, s, oldID, Found, "old")
				getWant(t, s, abc, want, value)
				if e = s.Validate(context.Background()); e != nil {
					t.Fatal(e)
				}
			})
		}
	}
}

func TestWaitingWriterReroutesAfterSplit(t *testing.T) {
	s := newLocal(t)
	var b strings.Builder
	for i := 0; i < 512; i++ {
		fmt.Fprintf(&b, "%s old\n", testID(i))
	}
	writeFile(t, s.path, b.String())
	original := system
	defer func() { system = original }()
	staging, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	system.Mkdirat = func(fd int, name string, mode uint32) error {
		e := original.Mkdirat(fd, name, mode)
		if e == nil && name == ".locks.tmp" {
			once.Do(func() { close(staging); <-release })
		}
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	outcomes := make(chan Outcome, 2)
	go func() { r, e := s.Put(ctx, testID(512), "first", Create); outcomes <- Outcome{r, e} }()
	<-staging
	go func() { r, e := s.Put(ctx, testID(513), "second", Create); outcomes <- Outcome{r, e} }()
	select {
	case out := <-outcomes:
		close(release)
		t.Fatal("writer completed while root publication was blocked", out)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		out := <-outcomes
		if out.Err != nil || out.Result.Status != Created {
			t.Fatal(out)
		}
	}
	getWant(t, s, testID(512), Found, "first")
	getWant(t, s, testID(513), Found, "second")
}

func TestPublicationHelper(t *testing.T) {
	phase := os.Getenv("LOCKS_TEST_PHASE")
	if phase == "" {
		return
	}
	original := system
	stop := func() { fmt.Println("READY"); time.Sleep(time.Minute) }
	system.Mkdirat = func(fd int, name string, mode uint32) error {
		e := original.Mkdirat(fd, name, mode)
		if e == nil && phase == "stage" && strings.HasSuffix(name, ".tmp") {
			stop()
		}
		return e
	}
	system.Renameat = func(fd int, from string, tofd int, to string) error {
		e := original.Renameat(fd, from, tofd, to)
		if e == nil && ((phase == "backup" && strings.HasSuffix(to, ".old")) || (phase == "publish" && strings.HasSuffix(from, ".tmp"))) {
			stop()
		}
		return e
	}
	system.Unlinkat = func(fd int, name string, flags int) error {
		e := original.Unlinkat(fd, name, flags)
		if e == nil && phase == "cleanup" && strings.HasSuffix(name, ".old") {
			stop()
		}
		return e
	}
	TestProcessHelper(t)
}
