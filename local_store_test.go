//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newLocal(t *testing.T) *Local {
	t.Helper()
	// The macOS temporary directory may start with the /var symlink.
	parent, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s, e := OpenLocal(filepath.Join(parent, "locks"), LocalOptions{})
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
func TestRecoveryRetainsStageWithMalformedLiveData(t *testing.T) {
	for _, shape := range []string{"root-leaf", "root-tree", "shard"} {
		for _, operation := range []string{"recover", "put", "batch"} {
			t.Run(shape+"/"+operation, func(t *testing.T) {
				s := newLocal(t)
				parent, name := s.parent, s.name
				live := s.path
				if shape != "root-leaf" {
					if err := os.Mkdir(s.path, 0700); err != nil {
						t.Fatal(err)
					}
					live = filepath.Join(s.path, "XX")
					if shape == "shard" {
						parent, name = s.path, "XX"
					}
				}
				stage := filepath.Join(parent, "."+name+".tmp")
				writeFile(t, live, "BAD\n")
				writeFile(t, stage, "incomplete stage")
				ctx := context.Background()
				var err error
				switch operation {
				case "recover":
					err = s.Recover(ctx)
				case "put":
					_, err = s.Put(ctx, abc, "new", Replace)
				case "batch":
					var out []Outcome
					out, err = s.PutBatch(ctx, []Write{{abc, "new", Replace}})
					if err == nil && len(out) == 1 {
						err = out[0].Err
					}
				}
				requireErr(t, err, ErrFormat)
				for path, want := range map[string]string{live: "BAD\n", stage: "incomplete stage"} {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != want {
						t.Fatalf("evidence changed at %s: %q, %v", path, data, err)
					}
				}
			})
		}
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
	d, e := openParent(context.Background(), s.parent)
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
		d, e := openParent(context.Background(), s.parent)
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
	if _, e = io.ReadFull(stdout, buf); e != nil {
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

func TestLocalNativeCaseAlias(t *testing.T) {
	s := newLocal(t)
	if err := os.Mkdir(s.path, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.path, "xx"), string(abc)+" old\n")
	for _, read := range []func() error{
		func() error { _, err := s.Get(context.Background(), abc); return err },
		func() error { _, err := s.Put(context.Background(), abc, "new", Replace); return err },
		func() error { return s.Recover(context.Background()) },
	} {
		requireErr(t, read(), ErrFormat)
	}
	b, err := os.ReadFile(filepath.Join(s.path, "xx"))
	if err != nil || string(b) != string(abc)+" old\n" {
		t.Fatal("alias was modified", err)
	}
}
