package locks

import (
	"context"
	"sync"
)

// Memory is an in-process Store. Its zero value is ready for use.
type Memory struct {
	mu     sync.RWMutex
	values map[ID]string
}

func (m *Memory) Get(ctx context.Context, id ID) (Result, error) {
	if _, e := ParseID(string(id)); e != nil {
		return Result{}, e
	}
	if e := ctx.Err(); e != nil {
		return Result{}, e
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.values[id]
	s := Missing
	if ok {
		s = Found
	}
	return Result{id, s, v}, nil
}
func (m *Memory) Put(ctx context.Context, id ID, value string, mode Mode) (Result, error) {
	l, _ := (Limits{}).normalized()
	w, e := validateWrite(Write{id, value, mode}, l)
	if e != nil {
		return Result{}, e
	}
	if e = ctx.Err(); e != nil {
		return Result{}, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.values[id]
	r := decide(id, old, ok, w)
	if r.Status == Created || r.Status == Replaced {
		if m.values == nil {
			m.values = map[ID]string{}
		}
		m.values[id] = w.Value
	}
	return r, nil
}
