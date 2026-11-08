package lockfile

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

// TryExisting never creates a lock or scope binding, and never waits. On success
// the caller can hold exclusion briefly while reading its associated metadata.
// ErrBusy proves only contention at the probe, not a worker identity or health.
func TryExisting(ctx context.Context, root, path string) (*Held, error) {
	d, err := ownedfs.OpenKnown(ctx, root, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	f, err := d.OpenFile(ctx, filepath.Base(path), os.O_RDWR, 0)
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	err = flock(ctx, f, path, syscall.LOCK_EX, false, func() error {
		current, err := d.Stat(ctx, filepath.Base(path))
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(current, opened) {
			return ownedfs.ErrScope
		}
		return d.Validate(ctx)
	})
	if err != nil {
		_ = f.Close()
		_ = d.Close()
		return nil, err
	}
	return &Held{file: f, dir: d}, nil
}
