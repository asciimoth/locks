//go:build windows

package locks

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsRootPaths(t *testing.T) {
	for _, path := range []string{`C:\`, `\\host\share`, `\\.\C:\locks`, `\\?\C:\locks`, `C:\locks:stream`, `C:\work.\locks`, `C:\work\locks `, `C:\work\NUL.txt`, `C:\.locks.GUARD`} {
		_, err := OpenLocal(path, LocalOptions{})
		requireErr(t, err, ErrFormat)
	}
	for _, path := range []string{`C:\locks`, `\\host\share\dir\locks`} {
		if _, err := OpenLocal(path, LocalOptions{}); err != nil {
			t.Fatal(path, err)
		}
	}
}

func TestWindowsRejectsJunctions(t *testing.T) {
	s := newLocal(t)
	target := filepath.Join(s.parent, "elsewhere")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "XX"), string(abc)+" outside\n")
	makeTestJunction(t, s.path, target)
	_, err := s.Get(context.Background(), abc)
	requireErr(t, err, ErrFormat)
	_, err = s.Put(context.Background(), abc, "new", Replace)
	requireErr(t, err, ErrFormat)
	sub, err := OpenLocal(filepath.Join(s.path, "XX"), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sub.Get(context.Background(), abc)
	requireErr(t, err, ErrFormat)
}

// Create a directory junction without requiring symbolic-link privileges.
func makeTestJunction(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	// REPARSE_DATA_BUFFER has an eight-byte header, then the two UTF-16
	// name ranges (offset and length in bytes, excluding the terminators).
	names := append(substitute, printName...)
	buf := make([]byte, 16+2*len(names))
	binary.LittleEndian.PutUint32(buf, windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:], uint16(len(buf)-8))
	binary.LittleEndian.PutUint16(buf[10:], uint16((len(substitute)-1)*2))
	binary.LittleEndian.PutUint16(buf[12:], uint16(len(substitute)*2))
	binary.LittleEndian.PutUint16(buf[14:], uint16((len(printName)-1)*2))
	for i, ch := range names {
		binary.LittleEndian.PutUint16(buf[16+2*i:], ch)
	}
	var returned uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &returned, nil); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			t.Skipf("test filesystem cannot create directory junctions: %v", err)
		}
		t.Fatal(err)
	}
}

func TestWindowsSharingViolation(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	path, err := windows.UTF16PtrFromString(s.path)
	if err != nil {
		t.Fatal(err)
	}
	hold := func() windows.Handle {
		h, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := hold()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err = s.Put(ctx, abc, "new", Replace)
	cancel()
	if closeErr := windows.CloseHandle(h); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	getWant(t, s, abc, Found, "old")
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	h = hold()
	released := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		released <- windows.CloseHandle(h)
	}()
	putWant(t, s, abc, "new", Replace, Replaced)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	getWant(t, s, abc, Found, "new")
}

func TestWindowsReplacementKeepsDACL(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	readDACL := func(path string) string {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if sd.String() == "" {
			t.Fatal("empty security descriptor")
		}
		return sd.String()
	}
	// An independently installed ACL must survive replacing the file.
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FR;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(s.path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before := readDACL(s.path)
	putWant(t, s, abc, "new", Replace, Replaced)
	if got := readDACL(s.path); got != before {
		t.Fatalf("DACL changed from %s to %s", before, got)
	}
}
