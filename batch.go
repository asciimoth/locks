package locks

import "context"

// BatchStore can group requests to reduce leaf scans and network requests. A
// returned error applies to the whole batch; per-key failures are retained in Outcome.
// A batch write error can be indeterminate if earlier sub-batches committed.
// With no batch error, return one Outcome per input item in input order.
// Successful outcomes must contain the requested ID.
type BatchStore interface {
	Store
	GetBatch(context.Context, []ID) ([]Outcome, error)
	PutBatch(context.Context, []Write) ([]Outcome, error)
}

func validateBatch(req Request, l Limits) (Request, error) {
	if len(req.Items) == 0 {
		return Request{}, ErrFormat
	}
	if len(req.Items) > l.Entries {
		return Request{}, &TooManyError{l.Entries}
	}
	req.Items = append([]Write(nil), req.Items...)
	seen := map[ID]bool{}
	for i, w := range req.Items {
		if _, e := ParseID(string(w.ID)); e != nil {
			return Request{}, e
		}
		if seen[w.ID] {
			return Request{}, ErrFormat
		}
		seen[w.ID] = true
		if req.Put {
			v, e := validateWrite(w, l)
			if e != nil {
				return Request{}, e
			}
			req.Items[i] = v
		}
	}
	return req, nil
}

// GetBatch scans each selected local leaf once and returns results in input order.
func (s *Local) GetBatch(ctx context.Context, ids []ID) ([]Outcome, error) {
	req := Request{Items: make([]Write, len(ids))}
	for i, id := range ids {
		req.Items[i].ID = id
	}
	req, e := validateBatch(req, s.limits)
	if e != nil {
		return nil, e
	}
	return s.batchOperation(ctx, req)
}

// PutBatch validates all input before writing and groups edits by leaf. Conflicts
// remain per-key results. Other keys can still commit when one leaf fails.
func (s *Local) PutBatch(ctx context.Context, items []Write) ([]Outcome, error) {
	if s.options.ReadOnly {
		return nil, ErrDenied
	}
	req, e := validateBatch(Request{Put: true, Items: items}, s.limits)
	if e != nil {
		return nil, e
	}
	for _, w := range req.Items {
		if _, e := validateLocalWrite(w, s.limits); e != nil {
			return nil, e
		}
	}
	return s.batchOperation(ctx, req)
}
