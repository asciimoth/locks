//go:build !darwin && !linux && !openbsd && !windows

package locks

import "context"

func prepareParent(context.Context, string) error { return ErrUnsupported }
