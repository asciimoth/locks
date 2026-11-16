//go:build darwin || linux || openbsd || windows

package locks

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
)

// mergeNode works from the deepest directories upward. A directory that cannot
// fit in one leaf also prevents its parent from fitting in one leaf.
func (s *Local) mergeNode(d *directory, name, p string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("merge node %q: %w", name, err)
		}
	}()
	if err = d.ctx.Err(); err != nil {
		return err
	}
	st, err := d.stat(name)
	if err != nil || st == nil || !st.dir {
		return err
	}
	sub, err := d.child(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeMergeFile(&sub.f)) }()
	entries, err := sub.f.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err = s.mergeNode(sub, entry.Name(), p+entry.Name()); err != nil {
			return err
		}
	}
	var records []Record
	bytes := 0
	for _, entry := range entries {
		info, e := sub.stat(entry.Name())
		if e != nil {
			return e
		}
		if info == nil {
			return ErrFormat
		}
		if info.dir {
			return nil
		}
		if info.links != 1 {
			return fmt.Errorf("%w: multiply linked leaf %q", ErrFormat, entry.Name())
		}
		leaf, e := sub.leaf(entry.Name(), p+entry.Name(), s.limits)
		if e != nil {
			return e
		}
		if len(records)+len(leaf.Records) > 512 {
			return nil
		}
		for _, record := range leaf.Records {
			n := len(record.ID) + 1 + len(record.Value) + 1
			if n > s.limits.ScanBytes-bytes {
				return nil
			}
			bytes += n
		}
		records = append(records, leaf.Records...)
	}
	// An empty directory uses its own metadata. With children, preserve the
	// first leaf's metadata and require equivalent access on every other node.
	source, err := sub.reopen()
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if err = closeMergeFile(&source); err != nil {
			return err
		}
		source, err = sub.open(entries[0].Name(), false)
		if err != nil {
			return err
		}
	}
	defer func() { err = errors.Join(err, closeMergeFile(&source)) }()
	access, err := mergeMetadata(source, false)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		expected, e := mergeMetadata(source, true)
		if e != nil {
			return e
		}
		actual, e := mergeMetadata(sub.f, false)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(expected, actual) {
			return fmt.Errorf("%w: directory and leaf metadata differ", ErrDenied)
		}
	}
	for i, entry := range entries {
		if i == 0 {
			continue
		}
		f, e := sub.open(entry.Name(), false)
		if e != nil {
			return e
		}
		actual, e := mergeMetadata(f, false)
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(access, actual) {
			return fmt.Errorf("%w: leaf metadata differ at %q", ErrDenied, entry.Name())
		}
	}
	info, err := source.Stat()
	if err != nil {
		return err
	}
	old, tmp := "."+name+".old", "."+name+".tmp"
	// Until the directory moves, the live tree is authoritative and recovery
	// may discard an incomplete .old file. After that move, .old is complete.
	if err = s.stage(d, old, p, nil, records, source, uint32(info.Mode().Perm())); err != nil {
		return err
	}
	// Close handles before renaming or deleting the directory on Windows.
	if err = errors.Join(closeMergeFile(&source), closeMergeFile(&sub.f)); err != nil {
		return err
	}
	if err = d.ctx.Err(); err != nil {
		return err
	}
	if err = d.rename(name, tmp); err != nil {
		return err
	}
	if err = d.rename(old, name); err != nil {
		return errors.Join(ErrIndeterminate, err)
	}
	if err = removeTree(d, tmp); err != nil {
		return errors.Join(ErrIndeterminate, err)
	}
	return nil
}

// mergeAccess contains all metadata that could change access to merged records.
// The platform implementation also includes copied extended attributes.
type mergeAccess struct {
	Mode         uint32
	Owner, Group uint64
	ACL          string
	Attributes   map[string]string
}

// Keep close errors without reporting a second close of an already closed file.
func closeMergeFile(f **os.File) error {
	if *f == nil {
		return nil
	}
	err := (*f).Close()
	*f = nil
	return err
}
