//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const localSupported = true

// A directory handle anchors all child operations to the directory we checked.
type directory struct {
	f   *os.File
	dev uint64
	ctx context.Context
}

type nodeInfo struct {
	key   identity
	dir   bool
	links uint64
	mode  uint32
}

// checkName rejects case aliases, including an alias beside an exact match.
// Use a new directory handle so repeated checks and concurrent readers do not
// share an enumeration offset. Check only this directory, not its descendants.
// As with other namespace checks, external edits must stop during operations.
func (d *directory) checkName(name string) error {
	return d.checkNameRetry(name, true)
}

func (d *directory) checkNameRetry(name string, retry bool) (err error) {
	f, e := d.reopen()
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	exact := false
	for {
		names, e := f.Readdirnames(128)
		for _, actual := range names {
			exact = exact || actual == name
			if actual != name && strings.EqualFold(actual, name) {
				return fmt.Errorf("%w: case alias %q for %q", ErrFormat, actual, name)
			}
		}
		if errors.Is(e, io.EOF) {
			if !exact {
				// Filesystems can equate names beyond Unicode simple case
				// folding. Never accept such an alias as the requested entry.
				st, err := d.rawStat(name)
				if err != nil {
					return err
				}
				if st != nil {
					// Another process can create the guard after the scan.
					// Read fresh entries before reporting an alias.
					if retry {
						return d.checkNameRetry(name, false)
					}
					return fmt.Errorf("%w: no exact entry for %q", ErrFormat, name)
				}
			}
			return nil
		}
		if e != nil {
			return e
		}
	}
}

func (d *directory) stat(name string) (*nodeInfo, error) {
	if err := d.checkName(name); err != nil {
		return nil, err
	}
	return d.rawStat(name)
}

func (d *directory) child(name string) (*directory, error) {
	f, e := d.open(name, true)
	if e != nil {
		return nil, e
	}
	return &directory{f, d.dev, d.ctx}, nil
}
func (d *directory) leaf(name, p string, l Limits) (leaf *Leaf, err error) {
	f, e := d.open(name, false)
	if e != nil {
		return nil, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return DecodeLeaf(f, p, l)
}

func slotFor(id ID) int {
	p := prefix(id)
	return 1 + 32*strings.IndexByte(alphabet, p[0]) + strings.IndexByte(alphabet, p[1])
}
func sidecars(d *directory, name string) (bool, error) {
	found := false
	for _, suffix := range []string{".old", ".tmp"} {
		s, e := d.stat("." + name + suffix)
		if e != nil {
			return false, e
		}
		if s != nil {
			if suffix == ".old" && s.dir || !s.dir && s.links != 1 {
				return false, ErrFormat
			}
			found = true
		}
	}
	return found, nil
}
func (s *Local) operation(ctx context.Context, id ID, w *Write) (result Result, err error) {
	defer func() {
		if err != nil && w != nil && (result.Status == Created || result.Status == Replaced) {
			err = errors.Join(ErrIndeterminate, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, s.options.LockTimeout)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return Result{}, e
	}
	d, e := openParent(ctx, s.parent)
	if e != nil {
		return Result{}, e
	}
	defer func() { err = errors.Join(err, d.f.Close()) }()
	for {
		g, e := getGuard(d, "."+s.name+".guard", w != nil)
		if e != nil {
			return Result{}, e
		}
		if g == nil {
			// An immutable checkout can be read without write access to its parent.
			r, e := s.lookup(d, s.name, "", id, nil, false)
			again, ge := getGuard(d, "."+s.name+".guard", false)
			if ge != nil {
				return Result{}, ge
			}
			if again == nil {
				return r, e
			}
			if ce := again.close(); ce != nil {
				return Result{}, ce
			}
			continue
		}
		r, e := s.guarded(ctx, d, g, id, w)
		e = errors.Join(e, g.close())
		return r, e
	}
}
func (s *Local) guarded(ctx context.Context, d *directory, g *guard, id ID, w *Write) (r Result, err error) {
	if e := g.lock(ctx, 0, false); e != nil {
		return r, e
	}
	rootHeld := true
	defer func() {
		if rootHeld {
			err = errors.Join(err, g.unlock(0))
		}
	}()
	st, e := d.stat(s.name)
	if e != nil {
		return r, e
	}
	sides, e := sidecars(d, s.name)
	if e != nil {
		return r, e
	}
	if w != nil && (st == nil || !st.dir || sides) {
		rootHeld = false
		if e = g.unlock(0); e != nil {
			return r, e
		}
		if e = g.lock(ctx, 0, true); e != nil {
			return r, e
		}
		rootHeld = true
		if e = s.recoverNode(d, s.name, ""); e != nil {
			return r, e
		}
		st, e = d.stat(s.name)
		if e != nil {
			return r, e
		}
		if st == nil || !st.dir {
			return s.lookup(d, s.name, "", id, w, true)
		}
		// Release exclusive ownership before resuming the normal root/slot order.
		rootHeld = false
		if e = g.unlock(0); e != nil {
			return r, e
		}
		return s.guarded(ctx, d, g, id, w)
	}
	if st != nil && st.dir {
		slot := slotFor(id)
		if e = g.lock(ctx, slot, w != nil); e != nil {
			return r, e
		}
		defer func() { err = errors.Join(err, g.unlock(slot)) }()
	}
	return s.lookup(d, s.name, "", id, w, true)
}
func (s *Local) lookup(d *directory, name, p string, id ID, w *Write, guarded bool) (r Result, err error) {
	if w != nil && p != "" {
		if e := s.recoverNode(d, name, p); e != nil {
			return r, e
		}
	}
	st, e := d.stat(name)
	if e != nil {
		return r, e
	}
	old, e := d.stat("." + name + ".old")
	if e != nil {
		return r, e
	}
	tmp, e := d.stat("." + name + ".tmp")
	if e != nil {
		return r, e
	}
	if !guarded && (old != nil || tmp != nil) {
		return r, fmt.Errorf("%w: sidecars without guard", ErrFormat)
	}
	if old != nil && (old.dir || old.links != 1) {
		return r, ErrFormat
	}
	if tmp != nil && !tmp.dir && tmp.links != 1 {
		return r, ErrFormat
	}
	live := name
	if st == nil && old != nil {
		st = old
		live = "." + name + ".old"
	}
	if st != nil && st.dir {
		if len(p) >= 52 {
			return r, ErrFormat
		}
		child, e := d.child(live)
		if e != nil {
			return r, e
		}
		defer func() { err = errors.Join(err, child.f.Close()) }()
		c := component(prefix(id), len(p))
		return s.lookup(child, c, p+c, id, w, guarded)
	}
	leaf := &Leaf{}
	if st != nil {
		leaf, e = d.leaf(live, p, s.limits)
		if e != nil {
			return r, e
		}
	}
	if w == nil {
		r = Result{ID: id, Status: Missing}
		for _, rec := range leaf.Records {
			if rec.ID == id {
				r.Status = Found
				r.Value = rec.Value
			}
		}
		return r, nil
	}
	r, data, records := leaf.edit(*w)
	if r.Status == Same || r.Status == Conflict {
		return r, nil
	}
	if st != nil && st.links != 1 {
		return Result{}, fmt.Errorf("%w: multiply linked leaf", ErrFormat)
	}
	if e = s.publish(d, name, p, data, records, st); e != nil {
		return Result{}, e
	}
	return r, nil
}

// inspectTree checks staged containment before deletion. It does not interpret
// stage contents: an incomplete stage is never authoritative.
func inspectTree(d *directory, name string) error {
	return inspectTreeDepth(d, name, 0)
}

func inspectTreeDepth(d *directory, name string, depth int) (err error) {
	if e := d.ctx.Err(); e != nil {
		return e
	}
	st, e := d.stat(name)
	if e != nil || st == nil {
		return e
	}
	if !st.dir {
		if st.links != 1 {
			return ErrFormat
		}
		return nil
	}
	// A valid stage needs at most 51 directory levels. Bound incomplete
	// stages too, before opening another handle or removing any evidence.
	if depth >= 52 {
		return ErrLimit
	}
	sub, e := d.child(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	for {
		entries, e := sub.f.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, en := range entries {
			if e := inspectTreeDepth(sub, en.Name(), depth+1); e != nil {
				return e
			}
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}
func removeTree(d *directory, name string) (err error) {
	if e := d.ctx.Err(); e != nil {
		return e
	}
	st, e := d.stat(name)
	if e != nil || st == nil {
		return e
	}
	if !st.dir {
		return d.remove(name, false)
	}
	sub, e := d.child(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	for {
		// Deletion can change directory offsets. Start each batch with a
		// fresh cursor so no remaining entry can be skipped.
		f, e := sub.reopen()
		if e != nil {
			return e
		}
		entries, e := f.ReadDir(128)
		if errors.Is(e, io.EOF) {
			e = nil
		}
		if e = errors.Join(e, f.Close()); e != nil {
			return e
		}
		if len(entries) == 0 {
			break
		}
		for _, en := range entries {
			if e := removeTree(sub, en.Name()); e != nil {
				return e
			}
		}
	}
	return d.remove(name, true)
}
func (s *Local) recoverNode(d *directory, name, p string) error {
	oldName, tmpName := "."+name+".old", "."+name+".tmp"
	st, e := d.stat(name)
	if e != nil {
		return e
	}
	old, e := d.stat(oldName)
	if e != nil {
		return e
	}
	tmp, e := d.stat(tmpName)
	if e != nil {
		return e
	}
	if old != nil && (old.dir || old.links != 1) {
		return ErrFormat
	}
	if tmp != nil {
		if e = inspectTree(d, tmpName); e != nil {
			return e
		}
	}
	// Keep all recovery evidence when the authoritative node is malformed,
	// including a stage left before the old leaf was moved to its backup.
	if st != nil && (old != nil || tmp != nil) {
		if e = s.validateNode(d, name, p, false); e != nil {
			return e
		}
	}
	if old != nil {
		if st == nil {
			if _, e = d.leaf(oldName, p, s.limits); e != nil {
				return e
			}
			if e = d.rename(oldName, name); e != nil {
				return e
			}
			old = nil
		}
	}
	if old != nil {
		if e = d.remove(oldName, false); e != nil {
			return e
		}
	}
	if tmp != nil {
		return removeTree(d, tmpName)
	}
	return nil
}
func (s *Local) validateNode(d *directory, name, p string, recover bool) (err error) {
	if e := d.ctx.Err(); e != nil {
		return e
	}
	for _, suffix := range []string{".tmp", ".old"} {
		side := "." + name + suffix
		st, e := d.stat(side)
		if e != nil {
			return e
		}
		if st != nil {
			if suffix == ".old" && (st.dir || st.links != 1) {
				return ErrFormat
			}
			if e = inspectTree(d, side); e != nil {
				return e
			}
		}
	}
	if recover {
		if e := s.recoverNode(d, name, p); e != nil {
			return e
		}
	}
	st, e := d.stat(name)
	if e != nil {
		return e
	}
	if st == nil {
		old, e := d.stat("." + name + ".old")
		if e != nil {
			return e
		}
		if old == nil {
			return nil
		}
		if old.dir || old.links != 1 {
			return ErrFormat
		}
		_, e = d.leaf("."+name+".old", p, s.limits)
		return e
	}
	if !st.dir {
		_, e = d.leaf(name, p, s.limits)
		return e
	}
	if len(p) >= 52 {
		return ErrFormat
	}
	sub, e := d.child(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	names := map[string]bool{}
	for {
		entries, e := sub.f.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, en := range entries {
			n := en.Name()
			base, side, e := shardName(n, len(p))
			if e != nil {
				return e
			}
			if side != "" {
				if e = inspectTree(sub, n); e != nil {
					return e
				}
				if side == "old" {
					st, e := sub.stat(n)
					if e != nil {
						return e
					}
					if st == nil || st.dir {
						return ErrFormat
					}
				}
			}
			names[base] = true
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	for n := range names {
		if e = s.validateNode(sub, n, p+n, recover); e != nil {
			return e
		}
	}
	return nil
}
func (s *Local) maintenance(ctx context.Context, recover bool) (err error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.LockTimeout)
	defer cancel()
	d, e := openParent(ctx, s.parent)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, d.f.Close()) }()
	g, e := getGuard(d, "."+s.name+".guard", true)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, g.close()) }()
	if e = g.lock(ctx, 0, true); e != nil {
		return e
	}
	defer func() { err = errors.Join(err, g.unlock(0)) }()
	if recover {
		if e = s.recoverNode(d, s.name, ""); e != nil {
			return e
		}
	} else {
		if _, e = sidecars(d, s.name); e != nil {
			return e
		}
	}
	return s.validateNode(d, s.name, "", recover)
}

func (s *Local) stage(d *directory, name, p string, data []byte, records []Record, source *os.File, mode uint32) (err error) {
	split := len(records) > 512 && len(p) < 52
	if !split {
		if data == nil {
			var b strings.Builder
			if e := EncodeLeaf(&b, records); e != nil {
				return e
			}
			data = []byte(b.String())
		}
		if _, e := DecodeLeaf(strings.NewReader(string(data)), p, s.limits); e != nil {
			return e
		}
	}
	if split {
		sub, e := d.createDir(name)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, sub.f.Close()) }()
		groups := map[string][]Record{}
		var order []string
		for _, r := range records {
			c := component(prefix(r.ID), len(p))
			if _, ok := groups[c]; !ok {
				order = append(order, c)
			}
			groups[c] = append(groups[c], r)
		}
		for _, c := range order {
			if e = s.stage(sub, c, p+c, nil, groups[c], source, mode); e != nil {
				return e
			}
		}
		dirMode := mode | ((mode & 0444) >> 2)
		return setMetadata(source, sub.f, dirMode, true)
	}
	f, e := d.createFile(name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if data != nil {
		_, e = f.Write(data)
	} else {
		e = EncodeLeaf(f, records)
	}
	if e != nil {
		return e
	}
	return setMetadata(source, f, mode, false)
}
func (s *Local) publish(d *directory, name, p string, data []byte, records []Record, st *nodeInfo) (err error) {
	tmp, old := "."+name+".tmp", "."+name+".old"
	mode := uint32(s.options.FileMode.Perm())
	var source *os.File
	if st != nil {
		mode = st.mode
		f, e := d.open(name, false)
		if e != nil {
			return e
		}
		source = f
	}
	stageErr := s.stage(d, tmp, p, data, records, source, mode)
	if source != nil {
		stageErr = errors.Join(stageErr, source.Close())
	}
	if stageErr != nil {
		return stageErr
	}
	// Always use the backup protocol, for file edits as well as splits.
	if st != nil {
		if e := d.rename(name, old); e != nil {
			return e
		}
	}
	if e := d.rename(tmp, name); e != nil {
		return e
	}
	if st != nil {
		if e := d.remove(old, false); e != nil {
			return errors.Join(ErrIndeterminate, e)
		}
	}
	return nil
}
