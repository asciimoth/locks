//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
)

func (s *Local) inspect(ctx context.Context, repair, merge bool) (health Health, err error) {
	err = s.withRootLock(ctx, func(d *directory) error {
		// Validate every authoritative leaf before any repair.
		if err := s.validateNode(d, s.name, "", false); err != nil {
			return err
		}
		if repair {
			if err := s.validateNode(d, s.name, "", true); err != nil {
				return err
			}
		}
		if err := s.measureNode(d, s.name, "", repair, &health); err != nil {
			return err
		}
		if merge {
			return s.mergeNode(d, s.name, "")
		}
		return nil
	})
	return health, err
}

func (s *Local) measureNode(d *directory, name, p string, repair bool, h *Health) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("node %q: %w", name, err)
		}
	}()
	if err = d.ctx.Err(); err != nil {
		return err
	}
	for _, suffix := range []string{".tmp", ".old"} {
		st, e := d.stat("." + name + suffix)
		if e != nil {
			return e
		}
		if st != nil {
			h.Pending++
		}
	}
	st, err := d.stat(name)
	if err != nil {
		return err
	}
	leafName := name
	if st == nil {
		leafName = "." + name + ".old"
		st, err = d.stat(leafName)
		if err != nil || st == nil {
			return err
		}
	}
	if !st.dir {
		leaf, e := d.leaf(leafName, p, s.limits)
		if e != nil {
			return e
		}
		n := len(leaf.Records)
		h.Leaves++
		h.Records += n
		if n > h.Largest {
			h.Largest = n
		}
		if n > 512 && len(p) < 52 {
			h.Overfull++
			if repair {
				if st.links != 1 {
					return ErrFormat
				}
				return s.publish(d, name, p, nil, leaf.Records, st)
			}
		}
		return nil
	}
	sub, err := d.child(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	names := map[string]bool{}
	for {
		entries, e := sub.f.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, entry := range entries {
			base, _, e := shardName(entry.Name(), len(p))
			if e != nil {
				return e
			}
			names[base] = true
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	order := make([]string, 0, len(names))
	for name := range names {
		order = append(order, name)
	}
	sort.Strings(order)
	for _, name := range order {
		if err = s.measureNode(sub, name, p+name, repair, h); err != nil {
			return err
		}
	}
	return nil
}
