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
	var missing []string
	var d *directory
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		d, err = openParent(ctx, path)
		if err == nil {
			break
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, os.ErrNotExist) || parent == path {
			return nil, err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, d.f.Close())
		}
		child, err := d.createDir(missing[i])
		if errors.Is(err, os.ErrExist) {
			// Another writer can create this directory before us.
			child, err = d.child(missing[i])
		}
		err = errors.Join(err, d.f.Close())
		if err != nil {
			if child != nil {
				err = errors.Join(err, child.f.Close())
			}
			return nil, err
		}
		d = child
	}
	return d, nil
}
