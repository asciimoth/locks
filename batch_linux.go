//go:build linux

package locks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

type batchItem struct {
	index int
	write Write
}

func (s *Local) batchOperation(ctx context.Context, req Request) (out []Outcome, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.LockTimeout)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	d, e := openParent(s.parent)
	if e != nil {
		return nil, e
	}
	defer func() {
		e := d.f.Close()
		if e != nil && req.Put {
			e = errors.Join(ErrIndeterminate, e)
		}
		err = errors.Join(err, e)
	}()
	for {
		g, e := getGuard(d, "."+s.name+".guard", req.Put)
		if e != nil {
			return nil, e
		}
		items := make([]batchItem, len(req.Items))
		out = make([]Outcome, len(items))
		for i, w := range req.Items {
			items[i] = batchItem{i, w}
			out[i].Result.ID = w.ID
		}
		if g == nil {
			s.batchNode(d, s.name, "", items, false, false, out)
			again, e := getGuard(d, "."+s.name+".guard", false)
			if e != nil {
				return nil, e
			}
			if again == nil {
				return out, nil
			}
			if e = again.close(); e != nil {
				return nil, e
			}
			continue
		}
		e = s.batchGuarded(ctx, d, g, items, req.Put, out)
		e = errors.Join(e, g.close())
		if e != nil {
			if req.Put {
				e = errors.Join(ErrIndeterminate, e)
			}
			return nil, e
		}
		return out, nil
	}
}
func (s *Local) batchGuarded(ctx context.Context, d *directory, g *guard, items []batchItem, put bool, out []Outcome) (err error) {
	if e := g.lock(ctx, 0, false); e != nil {
		return e
	}
	held := true
	defer func() {
		if held {
			err = errors.Join(err, g.unlock(0))
		}
	}()
	st, e := d.stat(s.name)
	if e != nil {
		return e
	}
	sides, e := sidecars(d, s.name)
	if e != nil {
		return e
	}
	if put && (st == nil || st.Mode&unix.S_IFMT == unix.S_IFREG || sides) {
		held = false
		if e = g.unlock(0); e != nil {
			return e
		}
		if e = g.lock(ctx, 0, true); e != nil {
			return e
		}
		held = true
		if e = s.recoverNode(d, s.name, ""); e != nil {
			return e
		}
		st, e = d.stat(s.name)
		if e != nil {
			return e
		}
		if st != nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			held = false
			if e = g.unlock(0); e != nil {
				return e
			}
			return s.batchGuarded(ctx, d, g, items, put, out)
		}
	}
	if st == nil || st.Mode&unix.S_IFMT == unix.S_IFREG {
		s.batchNode(d, s.name, "", items, put, true, out)
		return nil
	}
	sub, e := d.child(s.name)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, sub.f.Close()) }()
	groups, order := groupItems(items, 0)
	for _, name := range order {
		group := groups[name]
		slot := slotFor(group[0].write.ID)
		if e = g.lock(ctx, slot, put); e != nil {
			failItems(group, out, e)
			continue
		}
		s.batchNode(sub, name, name, group, put, true, out)
		if e = g.unlock(slot); e != nil {
			return e
		}
	}
	return nil
}
func groupItems(items []batchItem, depth int) (map[string][]batchItem, []string) {
	groups := map[string][]batchItem{}
	var order []string
	for _, item := range items {
		name := component(prefix(item.write.ID), depth)
		if _, ok := groups[name]; !ok {
			order = append(order, name)
		}
		groups[name] = append(groups[name], item)
	}
	return groups, order
}
func failItems(items []batchItem, out []Outcome, err error) {
	for _, item := range items {
		out[item.index] = Outcome{Result: Result{ID: item.write.ID}, Err: err}
	}
}
func (s *Local) batchNode(d *directory, name, p string, items []batchItem, put, guarded bool, out []Outcome) {
	if e := s.batchNodeWork(d, name, p, items, put, guarded, out); e != nil {
		failItems(items, out, e)
	}
}
func (s *Local) batchNodeWork(d *directory, name, p string, items []batchItem, put, guarded bool, out []Outcome) (err error) {
	if put {
		if e := s.recoverNode(d, name, p); e != nil {
			return e
		}
	}
	st, e := d.stat(name)
	if e != nil {
		return e
	}
	old, e := d.stat("." + name + ".old")
	if e != nil {
		return e
	}
	tmp, e := d.stat("." + name + ".tmp")
	if e != nil {
		return e
	}
	if !guarded && (old != nil || tmp != nil) {
		return ErrFormat
	}
	if old != nil && (old.Mode&unix.S_IFMT != unix.S_IFREG || old.Nlink != 1) {
		return ErrFormat
	}
	live := name
	if st == nil && old != nil {
		st = old
		live = "." + name + ".old"
	}
	if st != nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if len(p) >= 52 {
			return ErrFormat
		}
		sub, e := d.child(live)
		if e != nil {
			return e
		}
		defer func() {
			e := sub.f.Close()
			if e != nil && put {
				e = errors.Join(ErrIndeterminate, e)
			}
			err = errors.Join(err, e)
		}()
		groups, order := groupItems(items, len(p))
		for _, c := range order {
			s.batchNode(sub, c, p+c, groups[c], put, guarded, out)
		}
		return nil
	}
	leaf := &Leaf{}
	if st != nil {
		leaf, e = d.leaf(live, p, s.limits)
		if e != nil {
			return e
		}
	}
	index := map[ID]int{}
	for i, r := range leaf.Records {
		index[r.ID] = i
	}
	changed := map[ID]string{}
	for _, item := range items {
		w := item.write
		idx, exists := index[w.ID]
		value := ""
		if exists {
			value = leaf.Records[idx].Value
		}
		if !put {
			status := Missing
			if exists {
				status = Found
			}
			out[item.index].Result = Result{w.ID, status, value}
			continue
		}
		result := decide(w.ID, value, exists, w)
		out[item.index].Result = result
		if result.Status == Created || result.Status == Replaced {
			changed[w.ID] = fmt.Sprintf("%s %s\n", w.ID, w.Value)
			if exists {
				leaf.Records[idx].Value = w.Value
			} else {
				leaf.Records = append(leaf.Records, Record{w.ID, w.Value})
				leaf.lines = append(leaf.lines, leafLine{id: w.ID})
			}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if st != nil && st.Nlink != 1 {
		return ErrFormat
	}
	var data strings.Builder
	for _, line := range leaf.lines {
		if replacement, ok := changed[line.id]; ok {
			if data.Len() > 0 && !strings.HasSuffix(data.String(), "\n") {
				data.WriteByte('\n')
			}
			data.WriteString(replacement)
		} else {
			data.WriteString(line.raw)
		}
	}
	if e = s.publish(d, name, p, []byte(data.String()), leaf.Records, st); e != nil {
		for _, item := range items {
			if _, ok := changed[item.write.ID]; ok {
				out[item.index] = Outcome{Result: Result{ID: item.write.ID}, Err: e}
			}
		}
	}
	return nil
}
