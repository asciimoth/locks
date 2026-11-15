//go:build darwin || linux || openbsd || windows

package locks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryStageDepthLimit(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	stage := filepath.Join(s.parent, ".locks.tmp")
	deep := stage
	for i := 0; i < 52; i++ {
		deep = filepath.Join(deep, "A")
	}
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(deep, "partial")
	writeFile(t, evidence, "incomplete")
	requireErr(t, s.Recover(context.Background()), ErrLimit)
	data, err := os.ReadFile(evidence)
	if err != nil || string(data) != "incomplete" {
		t.Fatal("recovery changed evidence", err)
	}
	getWant(t, s, abc, Found, "old")
}

func TestRecoveryLargeStage(t *testing.T) {
	s := newLocal(t)
	putWant(t, s, abc, "old", Create, Created)
	stage := filepath.Join(s.parent, ".locks.tmp")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	// Force several directory reads during both inspection and removal.
	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(stage, fmt.Sprintf("partial-%03d", i)), "partial")
	}
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("stage remains", err)
	}
	getWant(t, s, abc, Found, "old")
}

func TestRecoveryTraversalCancellation(t *testing.T) {
	s := newLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	d, err := openParent(ctx, s.parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.f.Close(); err != nil {
			t.Error(err)
		}
	})
	cancel()
	requireErr(t, inspectTree(d, ".locks.tmp"), context.Canceled)
	requireErr(t, removeTree(d, ".locks.tmp"), context.Canceled)
	requireErr(t, s.validateNode(d, s.name, "", false), context.Canceled)
}
