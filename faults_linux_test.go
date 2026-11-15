//go:build linux

package locks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMetadataPreservation(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	if e := unix.Setxattr(s.path, "user.locks-test", []byte("metadata"), 0); e != nil {
		t.Skip("xattrs unsupported", e)
	}
	putWant(t, s, abc, "new", Replace, Replaced)
	v := make([]byte, 32)
	n, e := unix.Getxattr(s.path, "user.locks-test", v)
	if e != nil || string(v[:n]) != "metadata" {
		t.Fatal(n, e)
	}
	// A named-user ACL has four entries and a mask. It must remain usable after a split.
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	for i, entry := range []struct {
		tag, perm uint16
		id        uint32
	}{{1, 6, ^uint32(0)}, {2, 4, uint32(os.Getuid() + 1)}, {4, 4, ^uint32(0)}, {16, 4, ^uint32(0)}, {32, 0, ^uint32(0)}} {
		at := 4 + i*8
		binary.LittleEndian.PutUint16(acl[at:], entry.tag)
		binary.LittleEndian.PutUint16(acl[at+2:], entry.perm)
		binary.LittleEndian.PutUint32(acl[at+4:], entry.id)
	}
	if e = unix.Setxattr(s.path, "system.posix_acl_access", acl, 0); e != nil {
		t.Skip("ACL unsupported", e)
	}
	var b strings.Builder
	for i := 0; i < 513; i++ {
		fmt.Fprintf(&b, "%s old\n", testID(i))
	}
	writeFile(t, s.path, b.String())
	putWant(t, s, testID(0), "new", Replace, Replaced)
	st, e := os.Stat(s.path)
	if e != nil || st.Mode().Perm()&0500 != 0500 {
		t.Fatal(st, e)
	}
	acl2 := make([]byte, len(acl))
	n, e = unix.Getxattr(s.path, "system.posix_acl_access", acl2)
	if e != nil || n != len(acl) {
		t.Fatal(n, e)
	}
	for at := 4; at+8 <= n; at += 8 {
		perm := binary.LittleEndian.Uint16(acl2[at+2:])
		if perm&4 != 0 && perm&1 == 0 {
			t.Fatal("ACL removed directory traversal")
		}
	}
	getWant(t, s, testID(0), Found, "new")
}

// Inject a single failure at every observed syscall boundary. Each fixture starts
// from a known state. After the error, ordinary recovery must preserve an old or
// a committed new value. This includes failures during recovery itself.
func TestFilesystemFaultBoundaries(t *testing.T) {
	original, originalXattrs := system, xattrs
	defer func() { system, xattrs = original, originalXattrs }()
	injected := errors.New("injected filesystem failure")
	fixtures := []string{"create", "replace", "split", "nested-split", "restore", "cleanup", "read", "validate", "batch-create", "batch-replace", "batch-split", "batch-read", "batch-restore"}
	for _, kind := range fixtures {
		t.Run(kind, func(t *testing.T) {
			run := func(failAt int) (int, error) {
				system, xattrs = original, originalXattrs
				s := newLocal(t)
				ctx := context.Background()
				live := s.path
				id := abc
				if kind == "nested-split" {
					_ = os.Mkdir(s.path, 0700)
					live = filepath.Join(s.path, "XX")
				}
				if kind != "create" && kind != "batch-create" {
					writeFile(t, live, string(id)+" old\n")
				}
				if kind == "split" || kind == "nested-split" || kind == "batch-split" {
					var b strings.Builder
					for i := 0; i < 513; i++ {
						key := ID(string(abc)[:52] + fmt.Sprintf(":test-%d", i))
						fmt.Fprintf(&b, "%s old\n", key)
						if i == 0 {
							id = key
						}
					}
					writeFile(t, live, b.String())
				}
				if kind == "replace" {
					if e := unix.Setxattr(live, "user.locks-test", []byte("metadata"), 0); e != nil && !errors.Is(e, unix.ENOTSUP) {
						t.Fatal(e)
					}
				}
				writeFile(t, filepath.Join(s.parent, ".locks.guard"), "")
				if kind == "restore" || kind == "batch-restore" {
					_ = os.Rename(s.path, filepath.Join(s.parent, ".locks.old"))
					_ = os.Mkdir(filepath.Join(s.parent, ".locks.tmp"), 0700)
					writeFile(t, filepath.Join(s.parent, ".locks.tmp", "AA"), "incomplete")
				}
				if kind == "cleanup" {
					writeFile(t, filepath.Join(s.parent, ".locks.old"), string(id)+" old\n")
					writeFile(t, filepath.Join(s.parent, ".locks.tmp"), "incomplete")
				}
				count := 0
				inject := func(dst, functions any) {
					table := reflect.ValueOf(dst).Elem()
					source := reflect.ValueOf(functions)
					for i := 0; i < table.NumField(); i++ {
						field := table.Field(i)
						fn := source.Field(i)
						name := table.Type().Field(i).Name
						field.Set(reflect.MakeFunc(fn.Type(), func(args []reflect.Value) []reflect.Value {
							count++
							if count != failAt {
								return fn.Call(args)
							}
							// A failed close still relinquishes the descriptor in this fault model.
							if name == "Close" {
								fn.Call(args)
							}
							out := make([]reflect.Value, fn.Type().NumOut())
							for j := range out {
								out[j] = reflect.Zero(fn.Type().Out(j))
							}
							out[len(out)-1] = reflect.ValueOf(injected)
							return out
						}))
					}
				}
				inject(&system, original)
				inject(&xattrs, originalXattrs)
				var err error
				switch kind {
				case "batch-read":
					var out []Outcome
					out, err = s.GetBatch(ctx, []ID{id})
					if err == nil {
						err = out[0].Err
					}
				case "batch-create", "batch-replace", "batch-split", "batch-restore":
					var out []Outcome
					out, err = s.PutBatch(ctx, []Write{{id, "new", Replace}})
					if err == nil {
						err = out[0].Err
					}
				case "read":
					_, err = s.Get(ctx, id)
				case "validate":
					err = s.Validate(ctx)
				case "restore", "cleanup":
					err = s.Recover(ctx)
				default:
					_, err = s.Put(ctx, id, "new", Replace)
				}
				system, xattrs = original, originalXattrs
				if failAt > 0 && err == nil {
					t.Fatalf("failure %d was ignored", failAt)
				}
				if e := s.Recover(ctx); e != nil {
					t.Fatalf("recovery after fault %d: %v", failAt, e)
				}
				r, e := s.Get(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				if r.Status != Found && !((kind == "create" || kind == "batch-create") && r.Status == Missing) {
					t.Fatalf("lost key after fault %d: %+v", failAt, r)
				}
				if r.Status == Found && r.Value != "old" && r.Value != "new" {
					t.Fatal(r)
				}
				return count, err
			}
			n, e := run(0)
			if e != nil {
				t.Fatal(e)
			}
			for i := 1; i <= n; i++ {
				_, _ = run(i)
			}
		})
	}
}

func TestReplacementDoesNotActivateInheritedACL(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	if e := os.Chmod(s.path, 0640); e != nil {
		t.Fatal(e)
	}
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	for i, entry := range []struct {
		tag, perm uint16
		id        uint32
	}{{1, 7, ^uint32(0)}, {2, 4, uint32(os.Getuid() + 1)}, {4, 5, ^uint32(0)}, {16, 5, ^uint32(0)}, {32, 0, ^uint32(0)}} {
		at := 4 + i*8
		binary.LittleEndian.PutUint16(acl[at:], entry.tag)
		binary.LittleEndian.PutUint16(acl[at+2:], entry.perm)
		binary.LittleEndian.PutUint32(acl[at+4:], entry.id)
	}
	if e := unix.Setxattr(s.parent, "system.posix_acl_default", acl, 0); e != nil {
		t.Skip("default ACL unsupported", e)
	}
	putWant(t, s, abc, "new", Replace, Replaced)
	_, e := unix.Getxattr(s.path, "system.posix_acl_access", nil)
	if !errors.Is(e, unix.ENODATA) {
		t.Fatal("replacement gained an ACL", e)
	}
}
