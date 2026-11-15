//go:build windows

package locks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	listDir  = windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES
)

// Open the drive or UNC share root once. Every later component is opened
// relative to the checked directory handle with reparse processing disabled.
func openParent(ctx context.Context, path string) (*directory, error) {
	volume := filepath.VolumeName(path)
	root := `\\?\` + volume + `\`
	if strings.HasPrefix(volume, `\\`) {
		root = `\\?\UNC\` + volume[2:] + `\`
	}
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, listDir, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), root)
	info, err := windowsNode(f)
	if err != nil || !info.dir {
		if err == nil {
			err = ErrFormat
		}
		return nil, errors.Join(err, f.Close())
	}
	d := &directory{f, info.key.dev, ctx}
	for _, name := range strings.Split(path[len(volume):], `\`) {
		if name == "" {
			continue
		}
		child, _, err := d.openAt(name, listDir, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
		err = errors.Join(err, d.f.Close())
		if err != nil {
			if child != nil {
				err = errors.Join(err, child.Close())
			}
			return nil, err
		}
		d.f = child
	}
	return d, nil
}

func windowsNode(f *os.File) (*nodeInfo, error) {
	h := windows.Handle(f.Fd())
	kind, err := windows.GetFileType(h)
	if err != nil {
		return nil, err
	}
	if kind != windows.FILE_TYPE_DISK {
		return nil, ErrFormat
	}
	var st windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &st); err != nil {
		return nil, err
	}
	if st.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		return nil, fmt.Errorf("%w: unsafe entry %s", ErrFormat, f.Name())
	}
	key := identity{dev: uint64(st.VolumeSerialNumber), ino: uint64(st.FileIndexHigh)<<32 | uint64(st.FileIndexLow)}
	// ReFS needs the full 128-bit ID. Older filesystems expose the legacy ID.
	var id struct {
		Volume uint64
		ID     [16]byte
	}
	err = windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id)))
	if err == nil {
		key = identity{dev: id.Volume, ino: binary.LittleEndian.Uint64(id.ID[:8]), high: binary.LittleEndian.Uint64(id.ID[8:])}
	} else if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) && !errors.Is(err, windows.ERROR_NOT_SUPPORTED) && !errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return nil, err
	}
	mode := uint32(0666)
	if st.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		mode = 0444
	}
	return &nodeInfo{key: key, dir: st.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0, links: uint64(st.NumberOfLinks), mode: mode}, nil
}

func dosError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

func (d *directory) openAt(name string, access, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (*os.File, *nodeInfo, error) {
	if !validWindowsName(name) {
		return nil, nil, ErrFormat
	}
	return d.openRelative(name, access, disposition, options, sd)
}

func (d *directory) openRelative(name string, access, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (*os.File, *nodeInfo, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, nil, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(d.f.Fd()), ObjectName: objectName,
		Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var io windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &io, nil, windows.FILE_ATTRIBUTE_NORMAL, shareAll,
		disposition, options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: name, Err: dosError(err)}
	}
	f := os.NewFile(uintptr(h), filepath.Join(d.f.Name(), name))
	info, err := windowsNode(f)
	if err == nil && info.key.dev != d.dev {
		err = ErrFormat
	}
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return f, info, nil
}

func (d *directory) reopen() (*os.File, error) {
	// An empty relative name opens the directory itself with a new cursor.
	// ReOpenFile fails for these directory handles. A duplicate shares the cursor.
	// Merge reads and copies the directory security descriptor.
	f, _, err := d.openRelative("", listDir|windows.READ_CONTROL, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	return f, err
}

func (d *directory) rawStat(name string) (*nodeInfo, error) {
	f, info, err := d.openAt(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return info, f.Close()
}

func (d *directory) open(name string, dir bool) (*os.File, error) {
	if err := d.checkName(name); err != nil {
		return nil, err
	}
	options, access := uint32(windows.FILE_NON_DIRECTORY_FILE), uint32(windows.FILE_GENERIC_READ)
	if dir {
		options, access = windows.FILE_DIRECTORY_FILE, listDir|windows.READ_CONTROL
	}
	f, _, err := d.openAt(name, access, windows.FILE_OPEN, options, nil)
	return f, err
}

func (d *directory) openGuard(name string, create, write bool) (*os.File, *nodeInfo, bool, error) {
	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE)
	disposition := uint32(windows.FILE_OPEN)
	var sd *windows.SECURITY_DESCRIPTOR
	if create {
		var err error
		sd, err = privateDescriptor(d.f)
		if err != nil {
			return nil, nil, false, err
		}
		disposition = windows.FILE_OPEN_IF // never truncate a concurrently created guard
	}
	f, st, err := d.openAt(name, access, disposition, windows.FILE_NON_DIRECTORY_FILE, sd)
	writable := true
	if !write && (errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_WRITE_PROTECT)) {
		f, st, err = d.openAt(name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
		writable = false
	}
	if err == nil && st.key.ino == 0 && st.key.high == 0 {
		err = errors.Join(ErrUnsupported, f.Close())
		return nil, nil, false, err
	}
	return f, st, writable, err
}

func tryRangeLock(f *os.File, slot int, exclusive bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{Offset: uint32(slot)})
}

func lockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

func unlockRange(f *os.File, slot int) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{Offset: uint32(slot)})
}

func (d *directory) createStage(name string, access, options uint32) (*os.File, error) {
	sd, err := privateDescriptor(d.f)
	if err != nil {
		return nil, err
	}
	f, _, err := d.openAt(name, access|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_CREATE, options, sd)
	return f, err
}

func (d *directory) createDir(name string) (*directory, error) {
	f, err := d.createStage(name, listDir, windows.FILE_DIRECTORY_FILE)
	if err != nil {
		return nil, err
	}
	return &directory{f, d.dev, d.ctx}, nil
}

func (d *directory) createFile(name string) (*os.File, error) {
	return d.createStage(name, windows.FILE_GENERIC_WRITE, windows.FILE_NON_DIRECTORY_FILE)
}

// Both opening the source for DELETE and the rename/disposition itself can hit
// a sharing violation. Retry only that error and preserve the protocol sidecars.
func (d *directory) changeEntry(name string, change func(windows.Handle, *nodeInfo) error) error {
	for {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		f, st, err := d.openAt(name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
		if err == nil {
			if !st.dir && st.links != 1 {
				err = ErrFormat
			} else {
				err = change(windows.Handle(f.Fd()), st)
			}
			if closeErr := f.Close(); closeErr != nil {
				if err == nil {
					return errors.Join(ErrIndeterminate, closeErr)
				}
				return errors.Join(err, closeErr)
			}
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-d.ctx.Done():
			timer.Stop()
			return errors.Join(d.ctx.Err(), err)
		case <-timer.C:
		}
	}
}

func (d *directory) rename(from, to string) error {
	if !validWindowsName(to) {
		return ErrFormat
	}
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	// Both v1 renames target an absent sibling in the same open directory.
	type renameInfo struct {
		Replace uint32
		Root    windows.Handle
		Length  uint32
		Name    [1]uint16
	}
	var header renameInfo
	n := len(name) - 1
	buf := make([]byte, max(int(unsafe.Sizeof(header)), int(unsafe.Offsetof(header.Name))+2*n))
	info := (*renameInfo)(unsafe.Pointer(&buf[0]))
	info.Root = windows.Handle(d.f.Fd())
	info.Length = uint32(2 * n)
	copy(unsafe.Slice(&info.Name[0], n), name[:n])
	err = d.changeEntry(from, func(h windows.Handle, _ *nodeInfo) error {
		var io windows.IO_STATUS_BLOCK
		return dosError(windows.NtSetInformationFile(h, &io, &buf[0], uint32(len(buf)), windows.FileRenameInformation))
	})
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

func (d *directory) remove(name string, dir bool) error {
	return d.changeEntry(name, func(h windows.Handle, info *nodeInfo) error {
		if dir != info.dir {
			return ErrFormat
		}
		var io windows.IO_STATUS_BLOCK
		if info.mode&0200 == 0 {
			flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
			return dosError(windows.NtSetInformationFile(h, &io, (*byte)(unsafe.Pointer(&flags)), 4, windows.FileDispositionInformationEx))
		}
		remove := byte(1)
		return dosError(windows.NtSetInformationFile(h, &io, &remove, 1, windows.FileDispositionInformation))
	})
}
