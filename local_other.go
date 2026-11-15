//go:build !darwin && !linux && !openbsd && !windows

package locks

import "context"

const localSupported = false

func (s *Local) operation(context.Context, ID, *Write) (Result, error) {
	return Result{}, ErrUnsupported
}
func (s *Local) maintenance(context.Context, bool) error { return ErrUnsupported }
func (s *Local) batchOperation(context.Context, Request) ([]Outcome, error) {
	return nil, ErrUnsupported
}
