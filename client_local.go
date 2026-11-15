//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// prepareParent creates missing directories without following links. Existing
// directories keep their permissions. The caller must trust the path's owners,
// as required for Local. Each new directory uses the local stage permissions.
func prepareParent(ctx context.Context, path string) error {
	d, err := createParents(ctx, path)
	if err != nil {
		return err
	}
	return d.f.Close()
}

func createParents(ctx context.Context, path string) (*directory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := openParent(ctx, path)
	if !errors.Is(err, os.ErrNotExist) {
		return d, err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return nil, err
	}
	d, err = createParents(ctx, parent)
	if err != nil {
		return nil, err
	}
	child, err := d.createDir(filepath.Base(path))
	if errors.Is(err, os.ErrExist) {
		// Another writer can create this directory before us.
		child, err = d.child(filepath.Base(path))
	}
	err = errors.Join(err, d.f.Close())
	if err != nil && child != nil {
		err = errors.Join(err, child.f.Close())
		child = nil
	}
	return child, err
}
