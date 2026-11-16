//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

type identity struct{ dev, ino, high uint64 }
type rangeOwner struct {
	readers int
	writer  bool
}
type guard struct {
	f          *os.File
	key        identity
	refs       int
	mu         sync.Mutex
	ranges     [1025]rangeOwner
	writable   bool
	noParallel bool
}

var guards = struct {
	sync.Mutex
	m map[identity]*guard
}{m: map[identity]*guard{}}

func getGuard(d *directory, name string, write bool) (*guard, error) {
	return getGuardMode(d, name, write, false)
}

func (s *Local) getGuard(d *directory, write bool) (*guard, error) {
	return getGuardMode(d, "."+s.name+".guard", write, s.options.NoParallel)
}

func getGuardMode(d *directory, name string, write, noParallel bool) (*guard, error) {
	guards.Lock()
	defer guards.Unlock()
	st, err := d.stat(name)
	if err != nil {
		return nil, err
	}
	if st == nil && !write {
		return nil, nil
	}
	if st != nil {
		if st.dir || st.links != 1 {
			return nil, ErrFormat
		}
		if g := guards.m[st.key]; g != nil {
			if g.noParallel != noParallel {
				return nil, fmt.Errorf("%w: conflicting NoParallel settings for the same store", ErrFormat)
			}
			if write && !g.writable {
				return nil, ErrDenied
			}
			g.refs++
			return g, nil
		}
	}
	f, opened, writable, err := d.openGuard(name, st == nil, write)
	if err != nil {
		return nil, err
	}
	if opened.dir || opened.links != 1 || (st != nil && opened.key != st.key) {
		return nil, errors.Join(ErrFormat, f.Close())
	}
	g := &guard{f: f, key: opened.key, refs: 1, writable: writable, noParallel: noParallel}
	guards.m[g.key] = g
	return g, nil
}

func (g *guard) close() error {
	guards.Lock()
	defer guards.Unlock()
	g.refs--
	if g.refs == 0 {
		delete(guards.m, g.key)
		return g.f.Close()
	}
	return nil
}
func (g *guard) lock(ctx context.Context, slot int, exclusive bool) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		g.mu.Lock()
		r := &g.ranges[slot]
		if !r.writer && (!exclusive || r.readers == 0) {
			var e error
			if r.readers == 0 && !g.noParallel {
				e = tryRangeLock(g.f, slot, exclusive)
			}
			if e == nil {
				if exclusive {
					r.writer = true
				} else {
					r.readers++
				}
				g.mu.Unlock()
				return nil
			}
			if !lockBusy(e) {
				g.mu.Unlock()
				return e
			}
		}
		g.mu.Unlock()
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (g *guard) unlock(slot int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := &g.ranges[slot]
	if r.writer {
		r.writer = false
	} else {
		r.readers--
	}
	if r.readers > 0 || g.noParallel {
		return nil
	}
	return unlockRange(g.f, slot)
}
