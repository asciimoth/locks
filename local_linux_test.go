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

func newLocal(t *testing.T) *Local {
	t.Helper()
	s, e := OpenLocal(filepath.Join(t.TempDir(), "locks"), LocalOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestLocalBasic(t *testing.T) {
	s := newLocal(t)
	ctx := context.Background()
	getWant(t, s, abc, Missing, "")
	if _, e := os.Stat(filepath.Join(s.parent, ".locks.guard")); !os.IsNotExist(e) {
		t.Fatal("optimistic read created guard", e)
	}
	putWant(t, s, abc, "\r", Create, Created)
	getWant(t, s, abc, Found, "")
	putWant(t, s, abc, "", Create, Same)
	putWant(t, s, abc, "v", Create, Conflict)
	putWant(t, s, abc, "v\r\r", Replace, Replaced)
	getWant(t, s, abc, Found, "v")
	if e := s.Validate(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	ro, e := OpenLocal(s.path, LocalOptions{ReadOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	getWant(t, ro, abc, Found, "v")
	_, e = ro.Put(ctx, abc, "", Create)
	requireErr(t, e, ErrDenied)
	requireErr(t, ro.Recover(ctx), ErrDenied)
	_, e = s.Get(ctx, "bad")
	requireErr(t, e, ErrFormat)
	_, e = s.Put(ctx, abc, "\x00", Create)
	requireErr(t, e, ErrFormat)
	c, cancel := context.WithCancel(ctx)
	cancel()
	_, e = s.Get(c, abc)
	requireErr(t, e, context.Canceled)
	requireErr(t, s.Validate(c), context.Canceled)
	if e = os.Chmod(s.path, 0640); e != nil {
		t.Fatal(e)
	}
	putWant(t, s, abc, "new", Replace, Replaced)
	st, e := os.Stat(s.path)
	if e != nil || st.Mode().Perm() != 0640 {
		t.Fatal(st, e)
	}
}
func TestLocalConfiguration(t *testing.T) {
	for _, tt := range []struct {
		p   string
		o   LocalOptions
		err error
	}{{"relative", LocalOptions{}, ErrFormat}, {"/", LocalOptions{}, ErrFormat}, {"/tmp/.locks.guard", LocalOptions{}, ErrFormat}, {"/tmp/x", LocalOptions{Durable: true}, ErrUnsupported}, {"/tmp/x", LocalOptions{Limits: Limits{ScanBytes: -1}}, ErrLimit}, {"/tmp/x", LocalOptions{LockTimeout: -1}, ErrLimit}, {"/tmp/x", LocalOptions{FileMode: os.ModeSymlink}, ErrFormat}} {
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
func TestLocalSplitThresholds(t *testing.T) {
	s := newLocal(t)
	ctx := context.Background()
	var b strings.Builder
	b.WriteString("# comment\n")
	for i := 0; i < 511; i++ {
		fmt.Fprintf(&b, "%s %d\n", testID(i), i)
	}
	writeFile(t, s.path, b.String())
	putWant(t, s, testID(511), "511", Create, Created)
	st, _ := os.Stat(s.path)
	if !st.Mode().IsRegular() {
		t.Fatal("split at 512")
	}
	putWant(t, s, testID(10), "changed", Replace, Replaced)
	st, _ = os.Stat(s.path)
	if !st.Mode().IsRegular() {
		t.Fatal("replace split at 512")
	}
	putWant(t, s, testID(512), "512", Create, Created)
	st, _ = os.Stat(s.path)
	if !st.IsDir() {
		t.Fatal("no split at 513")
	}
	for i := 0; i < 513; i++ {
		value := fmt.Sprint(i)
		if i == 10 {
			value = "changed"
		}
		getWant(t, s, testID(i), Found, value)
	}
	getWant(t, s, testID(9999), Missing, "")
	if e := s.Validate(ctx); e != nil {
		t.Fatal(e)
	}
	putWant(t, s, testID(9999), "later", Create, Created)
	putWant(t, s, testID(0), "0", Create, Same)
	var comments bool
	_ = filepath.Walk(s.path, func(p string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if !info.IsDir() {
			b, _ := os.ReadFile(p)
			comments = comments || strings.Contains(string(b), "# comment")
		}
		return nil
	})
	if comments {
		t.Fatal("split retained comment")
	}
}
func TestLocalTerminalSplit(t *testing.T) {
	s := newLocal(t)
	var records []Record
	digest := strings.Repeat("A", 52)
	for i := 0; i < 513; i++ {
		records = append(records, Record{ID(digest + fmt.Sprintf(":future-%d", i)), fmt.Sprint(i)})
	}
	var b strings.Builder
	if e := EncodeLeaf(&b, records); e != nil {
		t.Fatal(e)
	}
	writeFile(t, s.path, b.String())
	// Same does not rewrite a merged overfull leaf.
	putWant(t, s, records[0].ID, "0", Create, Same)
	st, _ := os.Stat(s.path)
	if st.IsDir() {
		t.Fatal("same split")
	}
	putWant(t, s, records[0].ID, "new", Replace, Replaced)
	path := filepath.Join(s.path, "AA")
	for i := 2; i < 52; i++ {
		path = filepath.Join(path, "A")
	}
	st, e := os.Stat(path)
	if e != nil || !st.Mode().IsRegular() {
		t.Fatal(st, e)
	}
	getWant(t, s, records[512].ID, Found, "512")
	if e = s.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
	putWant(t, s, ID(digest+":another"), "terminal", Create, Created)
	getWant(t, s, ID(digest+":another"), Found, "terminal")
}
func TestRecoveryStates(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, state := range []string{"stage", "backup", "published", "clean", "new-stage", "empty"} {
			t.Run(fmt.Sprint(nested, "/", state), func(t *testing.T) {
				s := newLocal(t)
				name, p, parent := s.name, "", s.parent
				if nested {
					if e := os.Mkdir(s.path, 0700); e != nil {
						t.Fatal(e)
					}
					name, p, parent = "XX", "XX", s.path
				}
				live := filepath.Join(parent, name)
				old := filepath.Join(parent, "."+name+".old")
				tmp := filepath.Join(parent, "."+name+".tmp")
				writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
				if state == "stage" || state == "clean" {
					writeFile(t, live, string(abc)+" old\n")
				}
				if state == "backup" || state == "published" {
					writeFile(t, old, string(abc)+" old\n")
				}
				if state == "stage" || state == "backup" || state == "new-stage" {
					if e := os.Mkdir(tmp, 0700); e != nil {
						t.Fatal(e)
					}
					writeFile(t, filepath.Join(tmp, "incomplete"), "partial")
				}
				if state == "published" {
					writeFile(t, live, string(abc)+" new\n")
				}
				status, value := Found, "old"
				if state == "published" {
					value = "new"
				}
				if state == "new-stage" || state == "empty" {
					status, value = Missing, ""
				}
				getWant(t, s, abc, status, value)
				if e := s.Recover(context.Background()); e != nil {
					t.Fatal(p, e)
				}
				getWant(t, s, abc, status, value)
				for _, path := range []string{old, tmp} {
					if _, e := os.Lstat(path); !os.IsNotExist(e) {
						t.Fatal("sidecar remains", path, e)
					}
				}
				putWant(t, s, abc, "after", Replace, map[bool]Status{true: Created, false: Replaced}[status == Missing])
				getWant(t, s, abc, Found, "after")
			})
		}
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
func TestLocalHardlinksAndBudgets(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	if e := os.Link(s.path, s.path+"-alias"); e != nil {
		t.Fatal(e)
	}
	_, e := s.Put(context.Background(), abc, "new", Replace)
	requireErr(t, e, ErrFormat)
	getWant(t, s, abc, Found, "old")
	putWant(t, s, abc, "old", Create, Same)
	_ = os.Remove(s.path + "-alias")
	small, _ := OpenLocal(s.path, LocalOptions{Limits: Limits{ScanBytes: 80}})
	_, e = small.Put(context.Background(), abc, strings.Repeat("x", 40), Replace)
	requireErr(t, e, ErrLimit)
	getWant(t, s, abc, Found, "old")
	if e = s.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestLocalThreadRace(t *testing.T) {
	s := newLocal(t)
	var wg sync.WaitGroup
	results := make(chan Outcome, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			other, _ := OpenLocal(s.path, LocalOptions{})
			r, e := other.Put(context.Background(), abc, fmt.Sprint(i), Create)
			results <- Outcome{r, e}
		}(i)
	}
	wg.Wait()
	close(results)
	created, conflicts := 0, 0
	for o := range results {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		if o.Result.Status == Created {
			created++
		} else if o.Result.Status == Conflict {
			conflicts++
		}
	}
	if created != 1 || conflicts != 39 {
		t.Fatal(created, conflicts)
	}
	guards.Lock()
	defer guards.Unlock()
	if len(guards.m) != 0 {
		t.Fatal("guard descriptor leak")
	}
}
func TestLockSlotsAndDeadlines(t *testing.T) {
	s := newLocal(t)
	_ = os.Mkdir(s.path, 0700)
	writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
	writeFile(t, filepath.Join(s.path, "XX"), string(abc)+" v\n")
	other := testID(1)
	writeFile(t, filepath.Join(s.path, string(other)[:2]), string(other)+" other\n")
	d, e := openParent(s.parent)
	if e != nil {
		t.Fatal(e)
	}
	defer d.f.Close()
	g, e := getGuard(d, ".locks.guard", true)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	ctx := context.Background()
	if e = g.lock(ctx, 0, false); e != nil {
		t.Fatal(e)
	}
	defer g.unlock(0)
	if e = g.lock(ctx, slotFor(abc), true); e != nil {
		t.Fatal(e)
	}
	defer g.unlock(slotFor(abc))
	getWant(t, s, other, Found, "other")
	putWant(t, s, other, "changed", Replace, Replaced)
	short, cancel := context.WithTimeout(ctx, 15*time.Millisecond)
	defer cancel()
	_, e = s.Get(short, abc)
	requireErr(t, e, context.DeadlineExceeded)
	// A second local reader cannot release the first reader's kernel ownership.
	slot := slotFor(other)
	if e = g.lock(ctx, slot, false); e != nil {
		t.Fatal(e)
	}
	if e = g.lock(ctx, slot, false); e != nil {
		t.Fatal(e)
	}
	if e = g.unlock(slot); e != nil {
		t.Fatal(e)
	}
	short2, cancel2 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel2()
	requireErr(t, g.lock(short2, slot, true), context.DeadlineExceeded)
	if e = g.unlock(slot); e != nil {
		t.Fatal(e)
	}
}
func TestProcessHelper(t *testing.T) {
	if os.Getenv("LOCKS_TEST_CHILD") != "1" {
		return
	}
	s, e := OpenLocal(os.Getenv("LOCKS_TEST_PATH"), LocalOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if hold := os.Getenv("LOCKS_TEST_HOLD"); hold != "" {
		d, e := openParent(s.parent)
		if e != nil {
			t.Fatal(e)
		}
		defer d.f.Close()
		g, e := getGuard(d, ".locks.guard", true)
		if e != nil {
			t.Fatal(e)
		}
		defer g.close()
		if e = g.lock(context.Background(), 0, true); e != nil {
			t.Fatal(e)
		}
		fmt.Println("READY")
		time.Sleep(time.Minute)
		return
	}
	if phase := os.Getenv("LOCKS_TEST_PHASE"); phase != "" {
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
	}
	r, e := s.Put(context.Background(), abc, os.Getenv("LOCKS_TEST_VALUE"), Create)
	if e != nil {
		t.Fatal(e)
	}
	fmt.Println(r.Status)
}
func TestLocalProcessRace(t *testing.T) {
	for _, equal := range []bool{false, true} {
		t.Run(fmt.Sprint(equal), func(t *testing.T) {
			s := newLocal(t)
			cmds := make([]*exec.Cmd, 2)
			var outputs [2]strings.Builder
			for i := range cmds {
				value := fmt.Sprint(i)
				if equal {
					value = "same"
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
				cmd.Env = append(os.Environ(), "LOCKS_TEST_CHILD=1", "LOCKS_TEST_PATH="+s.path, "LOCKS_TEST_VALUE="+value)
				cmd.Stdout = &outputs[i]
				cmd.Stderr = &outputs[i]
				cmds[i] = cmd
				if e := cmd.Start(); e != nil {
					t.Fatal(e)
				}
			}
			for _, cmd := range cmds {
				if e := cmd.Wait(); e != nil {
					t.Fatal(e, outputs)
				}
			}
			both := outputs[0].String() + outputs[1].String()
			want := "conflict"
			if equal {
				want = "same"
			}
			if strings.Count(both, "created") != 1 || !strings.Contains(both, want) {
				t.Fatal(both)
			}
		})
	}
}
func TestProcessDeathReleasesGuard(t *testing.T) {
	s := newLocal(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
	cmd.Env = append(os.Environ(), "LOCKS_TEST_CHILD=1", "LOCKS_TEST_PATH="+s.path, "LOCKS_TEST_HOLD=1")
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	buf := make([]byte, 6)
	if _, e = stdout.Read(buf); e != nil {
		t.Fatal(e)
	}
	if string(buf) != "READY\n" {
		t.Fatal(string(buf))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, e = s.Get(ctx, abc)
	requireErr(t, e, context.DeadlineExceeded)
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	putWant(t, s, abc, "after death", Create, Created)
}
func TestGuardIdentityAndUnsafeTypes(t *testing.T) {
	s := newLocal(t)
	d, e := openParent(s.parent)
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
			d, e := openParent(s.parent)
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
				cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
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

func TestShortSidecarNames(t *testing.T) {
	for _, name := range []string{".tmp", ".old", "..tmp"} {
		s := newLocal(t)
		if e := os.Mkdir(s.path, 0700); e != nil {
			t.Fatal(e)
		}
		writeFile(t, filepath.Join(s.path, name), "evidence")
		requireErr(t, s.Validate(context.Background()), ErrFormat)
		requireErr(t, s.Recover(context.Background()), ErrFormat)
		b, e := os.ReadFile(filepath.Join(s.path, name))
		if e != nil || string(b) != "evidence" {
			t.Fatal("unrecognized file changed", e)
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
