//go:build linux

package locks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Emulate case-insensitive name resolution at the syscall boundary. Directory
// reads retain the stored spelling, as they do on a case-preserving filesystem.
// These tests must not run in parallel because system is process-wide.
func emulateCaseFold(t *testing.T) {
	t.Helper()
	original := system
	t.Cleanup(func() { system = original })
	resolve := func(fd int, name string) string {
		path := fmt.Sprintf("/proc/self/fd/%d", fd)
		entries, err := os.ReadDir(path)
		if err != nil {
			return name
		}
		for _, en := range entries {
			if en.Name() == name {
				return name
			}
		}
		for _, en := range entries {
			if strings.EqualFold(en.Name(), name) {
				return en.Name()
			}
		}
		return name
	}
	system.Openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		return original.Openat(fd, resolve(fd, name), flags, mode)
	}
	system.Fstatat = func(fd int, name string, st *unix.Stat_t, flags int) error {
		return original.Fstatat(fd, resolve(fd, name), st, flags)
	}
}

func caseOperations() map[string]func(*Local) error {
	ctx := context.Background()
	batchErr := func(out []Outcome, err error) error {
		for _, item := range out {
			err = errors.Join(err, item.Err)
		}
		return err
	}
	return map[string]func(*Local) error{
		"get":       func(s *Local) error { _, err := s.Get(ctx, abc); return err },
		"put":       func(s *Local) error { _, err := s.Put(ctx, abc, "new", Replace); return err },
		"get-batch": func(s *Local) error { return batchErr(s.GetBatch(ctx, []ID{abc})) },
		"put-batch": func(s *Local) error {
			return batchErr(s.PutBatch(ctx, []Write{{ID: abc, Value: "new", Mode: Replace}}))
		},
		"validate": func(s *Local) error { return s.Validate(ctx) },
		"recover":  func(s *Local) error { return s.Recover(ctx) },
	}
}

// Capture names and file bytes to ensure rejected operations preserve the tree.
func caseSnapshot(t *testing.T, path string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(path, func(p string, en os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		if en.IsDir() {
			result[rel] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		result[rel] = "file:" + string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLocalCaseAliases(t *testing.T) {
	for _, folded := range []bool{false, true} {
		t.Run(fmt.Sprintf("folded=%v", folded), func(t *testing.T) {
			if folded {
				emulateCaseFold(t)
			}
			for _, location := range []string{"root", "first", "nested", "old", "tmp", "root-old", "root-tmp", "guard"} {
				for _, both := range []bool{false, true} {
					for opName, op := range caseOperations() {
						t.Run(fmt.Sprintf("%s/both=%v/%s", location, both, opName), func(t *testing.T) {
							s := newLocal(t)
							writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
							target := s.path
							switch location {
							case "first", "nested", "old", "tmp":
								if err := os.Mkdir(s.path, 0700); err != nil {
									t.Fatal(err)
								}
								target = filepath.Join(s.path, "XX")
								if location == "nested" {
									if err := os.Mkdir(target, 0700); err != nil {
										t.Fatal(err)
									}
									target = filepath.Join(target, "O")
								}
								if location == "old" || location == "tmp" {
									target = filepath.Join(s.path, ".XX."+location)
								}
							case "root-old":
								target = filepath.Join(s.parent, ".locks.old")
							case "root-tmp":
								target = filepath.Join(s.parent, ".locks.tmp")
							case "guard":
								target = filepath.Join(s.parent, ".locks.guard")
							}
							data := string(abc) + " old\n"
							if location == "guard" {
								data = ""
							}
							writeFile(t, target, data)
							base := filepath.Base(target)
							alias := strings.ToLower(base)
							if alias == base {
								alias = strings.ToUpper(base)
							}
							aliasPath := filepath.Join(filepath.Dir(target), alias)
							if both {
								writeFile(t, aliasPath, data)
								exactInfo, err := os.Stat(target)
								if err != nil {
									t.Fatal(err)
								}
								aliasInfo, err := os.Stat(aliasPath)
								if err != nil {
									t.Fatal(err)
								}
								if os.SameFile(exactInfo, aliasInfo) {
									t.Skip("filesystem cannot hold both spellings")
								}
							} else if err := os.Rename(target, aliasPath); err != nil {
								t.Fatal(err)
							}
							before := caseSnapshot(t, s.parent)
							requireErr(t, op(s), ErrFormat)
							after := caseSnapshot(t, s.parent)
							if !reflect.DeepEqual(before, after) {
								t.Fatalf("rejected operation changed files: before=%v after=%v", before, after)
							}
						})
					}
				}
			}
		})
	}
}

func TestExactNameChecksDoNotConsumeDirectory(t *testing.T) {
	path := t.TempDir()
	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(path, fmt.Sprintf("entry-%03d", i)), "")
	}
	writeFile(t, filepath.Join(path, "xx"), "")
	d, err := openParent(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	for i := 0; i < 3; i++ {
		requireErr(t, d.checkName("XX"), ErrFormat)
		if err := d.checkName("absent"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := d.f.ReadDir(-1)
	if err != nil || len(entries) != 301 {
		t.Fatalf("directory offset changed: entries=%d err=%v", len(entries), err)
	}
}

func TestExactNameCheckFailure(t *testing.T) {
	d, err := openParent(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	original := system
	t.Cleanup(func() { system = original })
	system.Openat = func(int, string, int, uint32) (int, error) { return -1, unix.EACCES }
	_, err = d.stat("XX")
	requireErr(t, err, unix.EACCES)
	_, err = d.open("XX", false)
	requireErr(t, err, unix.EACCES)
	_, err = getGuard(d, ".locks.guard", true)
	requireErr(t, err, unix.EACCES)
}

func TestExactNameCheckRejectsOtherFilesystemAliases(t *testing.T) {
	d, err := openParent(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	original := system
	t.Cleanup(func() { system = original })
	// Model a filesystem alias not covered by strings.EqualFold.
	system.Fstatat = func(int, string, *unix.Stat_t, int) error { return nil }
	requireErr(t, d.checkName("XX"), ErrFormat)
}

func TestCaseFoldCanonicalOperations(t *testing.T) {
	emulateCaseFold(t)
	s := newLocal(t)
	ctx := context.Background()
	getWant(t, s, abc, Missing, "")
	putWant(t, s, abc, "first", Create, Created)
	putWant(t, s, abc, "second", Replace, Replaced)
	getWant(t, s, abc, Found, "second")
	// Force a split, then exercise reads and writes through canonical shards.
	var b strings.Builder
	for i := 0; i < 513; i++ {
		fmt.Fprintf(&b, "%s old\n", testID(i))
	}
	writeFile(t, s.path, b.String())
	putWant(t, s, testID(0), "new", Replace, Replaced)
	for i := 0; i < 513; i++ {
		value := "old"
		if i == 0 {
			value = "new"
		}
		getWant(t, s, testID(i), Found, value)
	}
	getWant(t, s, abc, Missing, "")
	putWant(t, s, abc, "added", Create, Created)
	out, err := s.PutBatch(ctx, []Write{{ID: abc, Value: "batch", Mode: Replace}})
	if err != nil || len(out) != 1 || out[0].Err != nil || out[0].Result.Status != Replaced {
		t.Fatalf("batch: %v %v", out, err)
	}
	out, err = s.GetBatch(ctx, []ID{abc, testID(0)})
	if err != nil || len(out) != 2 || out[0].Err != nil || out[1].Err != nil || out[0].Result.Value != "batch" || out[1].Result.Value != "new" {
		t.Fatalf("batch: %v %v", out, err)
	}
	if err := s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCaseFoldCanonicalRecovery(t *testing.T) {
	emulateCaseFold(t)
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			s := newLocal(t)
			dir, name := s.parent, s.name
			if nested {
				dir, name = filepath.Join(s.path, "XX"), "O"
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
			writeFile(t, filepath.Join(dir, "."+name+".old"), string(abc)+" saved\n")
			writeFile(t, filepath.Join(dir, "."+name+".tmp"), "incomplete")
			getWant(t, s, abc, Found, "saved")
			if err := s.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			getWant(t, s, abc, Found, "saved")
			for _, suffix := range []string{".old", ".tmp"} {
				if _, err := os.Stat(filepath.Join(dir, "."+name+suffix)); !os.IsNotExist(err) {
					t.Fatalf("sidecar remains: %v", err)
				}
			}
		})
	}
}

func TestPointLookupDoesNotVisitOtherShards(t *testing.T) {
	s := newLocal(t)
	if err := os.Mkdir(s.path, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.path, "XX"), string(abc)+" value\n")
	// A malformed sibling must not cause a point lookup to walk its contents.
	if err := os.Mkdir(filepath.Join(s.path, "AA"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.path, "AA", "invalid"), "bad")
	getWant(t, s, abc, Found, "value")
	requireErr(t, s.Validate(context.Background()), ErrFormat)
}

func TestExactNameCheckConcurrentCreation(t *testing.T) {
	path := t.TempDir()
	d, err := openParent(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	original := system
	t.Cleanup(func() { system = original })
	created := false
	system.Fstatat = func(fd int, name string, st *unix.Stat_t, flags int) error {
		if !created && name == ".locks.guard" {
			created = true
			writeFile(t, filepath.Join(path, name), "")
		}
		return original.Fstatat(fd, name, st, flags)
	}
	if err := d.checkName(".locks.guard"); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("did not exercise concurrent creation")
	}
}

// This test runs on an actual case-insensitive filesystem when the temporary
// directory supports Linux casefold directories. Other hosts run the mandatory
// syscall-emulation tests above.
func TestNativeCaseFold(t *testing.T) {
	parent := t.TempDir()
	f, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Skipf("casefold directory flags unavailable: %v", err)
	}
	const fsCasefoldFlag = 0x400000
	if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, flags|fsCasefoldFlag); err != nil {
		t.Skipf("filesystem does not permit casefold directories: %v", err)
	}
	writeFile(t, filepath.Join(parent, "probe"), "")
	if _, err := os.Stat(filepath.Join(parent, "PROBE")); err != nil {
		t.Fatal("casefold flag did not enable folded lookup", err)
	}
	if err := os.Remove(filepath.Join(parent, "probe")); err != nil {
		t.Fatal(err)
	}
	for opName, op := range caseOperations() {
		t.Run(opName, func(t *testing.T) {
			dir, err := os.MkdirTemp(parent, "store-")
			if err != nil {
				t.Fatal(err)
			}
			s, err := OpenLocal(filepath.Join(dir, "locks"), LocalOptions{})
			if err != nil {
				t.Fatal(err)
			}
			putWant(t, s, abc, "old", Create, Created)
			if err := os.Remove(s.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(s.path, 0700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(s.path, "XX"), string(abc)+" old\n")
			getWant(t, s, abc, Found, "old")
			if err := os.Rename(filepath.Join(s.path, "XX"), filepath.Join(s.path, "xx")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(s.path, "XX")); err != nil {
				t.Fatal("shard directory is not case-insensitive", err)
			}
			before := caseSnapshot(t, dir)
			requireErr(t, op(s), ErrFormat)
			if after := caseSnapshot(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected operation changed store")
			}
		})
	}
}
