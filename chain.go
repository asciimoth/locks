package locks

import "context"

// ReadPolicy selects ordered precedence or comparison across all stores.
type ReadPolicy string

const (
	First  ReadPolicy = "first"
	Verify ReadPolicy = "verify"
)

// WritePolicy selects the first acknowledgement or all writable stores.
type WritePolicy string

const (
	WriteFirst WritePolicy = "first"
	WriteAll   WritePolicy = "all"
)

// Position couples a store with its independent trust and write policy.
type Position struct {
	Store               Store
	CheckOnly, ReadOnly bool
}

// Attempt records an actual operation, a skipped position, or an unattempted position.
type Attempt struct {
	Attempted, Skipped bool
	Result             Result
	Err                error
}

// Report retains all per-store outcomes. Partial writes never roll back earlier commits.
type Report struct {
	Result   Result
	Attempts []Attempt
	Err      error
}

// Chain is an ordered collection of stores. Its configuration must not change during use.
type Chain []Position

func (c Chain) Get(ctx context.Context, id ID, policy ReadPolicy) Report {
	out := Report{Result: Result{ID: id}, Attempts: make([]Attempt, len(c))}
	if e := contextError(ctx); e != nil {
		out.Err = e
		return out
	}
	if _, e := ParseID(string(id)); e != nil {
		out.Err = e
		return out
	}
	if policy != First && policy != Verify {
		out.Err = ErrFormat
		return out
	}
	eligible, miss := false, false
	var last error
	var observations []string
	for i, p := range c {
		if policy == First && p.CheckOnly {
			out.Attempts[i].Skipped = true
			continue
		}
		var r Result
		var e error
		if nilInput(p.Store) {
			e = ErrUnsupported
		} else {
			r, e = p.Store.Get(ctx, id)
		}
		if e == nil && (r.ID != id || r.Status != Found && r.Status != Missing) {
			e = ErrProtocol
		}
		out.Attempts[i] = Attempt{Attempted: true, Result: r, Err: e}
		if !p.CheckOnly {
			eligible = true
			if e != nil {
				last = e
			}
		}
		if e != nil {
			continue
		}
		if r.Status == Found {
			v, e := NormalizeValue(r.Value)
			if e != nil {
				out.Attempts[i].Err = e
				if !p.CheckOnly {
					last = e
				}
				continue
			}
			observations = append(observations, v)
			if !p.CheckOnly && out.Result.Status != Found {
				out.Result = Result{id, Found, v}
				if policy == First {
					return out
				}
			}
		} else if r.Status == Missing && !p.CheckOnly {
			miss = true
		}
	}
	if out.Result.Status == Found {
		for _, v := range observations {
			if v != out.Result.Value {
				out.Result = Result{ID: id, Status: Conflict}
				break
			}
		}
		return out
	}
	switch {
	case !eligible:
		out.Err = ErrNoSource
	case miss:
		out.Result.Status = Missing
	default:
		out.Err = last
	}
	return out
}
func (c Chain) Put(ctx context.Context, id ID, value string, mode Mode, policy WritePolicy) Report {
	out := Report{Result: Result{ID: id}, Attempts: make([]Attempt, len(c))}
	if e := contextError(ctx); e != nil {
		out.Err = e
		return out
	}
	l, _ := (Limits{}).normalized()
	w, e := validateWrite(Write{id, value, mode}, l)
	if e != nil {
		out.Err = e
		return out
	}
	if policy != WriteFirst && policy != WriteAll {
		out.Err = ErrFormat
		return out
	}
	last := ErrNoWritableStore
	success := false
	for i, p := range c {
		if p.ReadOnly {
			out.Attempts[i].Skipped = true
			continue
		}
		var r Result
		var e error
		if nilInput(p.Store) {
			e = ErrUnsupported
		} else {
			r, e = p.Store.Put(ctx, id, w.Value, mode)
		}
		out.Attempts[i] = Attempt{Attempted: true, Result: r, Err: e}
		if e != nil {
			last = e
			continue
		}
		if r.ID != id {
			last = ErrProtocol
			out.Attempts[i].Err = last
			continue
		}
		if r.Status == Conflict && mode == Create {
			out.Result = r
			return out
		}
		if r.Status != Created && r.Status != Same && !(r.Status == Replaced && mode == Replace) {
			last = ErrProtocol
			out.Attempts[i].Err = last
			continue
		}
		success = true
		out.Result = r
		if policy == WriteFirst {
			return out
		}
	}
	if !success {
		out.Err = last
	}
	return out
}

// WriteBack replicates only a successful source read, using create for every destination.
func (c Chain) WriteBack(ctx context.Context, read Report, policy WritePolicy) Report {
	if read.Err != nil {
		return Report{Err: read.Err}
	}
	if read.Result.Status != Found {
		return Report{Err: ErrNoSource}
	}
	return c.Put(ctx, read.Result.ID, read.Result.Value, Create, policy)
}
