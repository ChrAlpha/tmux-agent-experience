// Package lockfile provides advisory file locks whose waiting is bounded by a
// context.
package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

// retry is how often a contended lock is tried again.
const retry = 5 * time.Millisecond

// ErrBusy is returned by Try when another holder excludes this operation.
var ErrBusy = errors.New("lock busy")

// With runs fn while holding an exclusive flock on path, creating the file and
// its directory if needed. Waiting for the lock ends with ctx. Closing the file
// releases the lock, so it is released even if fn panics.
func With(ctx context.Context, path string, fn func() error) error {
	return with(ctx, path, syscall.LOCK_EX, true, fn)
}

// Shared participates in a stable namespace until fn returns. Every opener
// enters before opening a resource lock and leaves after closing that lock.
func Shared(ctx context.Context, path string, fn func() error) error {
	return with(ctx, path, syscall.LOCK_SH, true, fn)
}

// Try runs fn only when the exclusive lock can be acquired immediately.
func Try(ctx context.Context, path string, fn func() error) error {
	return with(ctx, path, syscall.LOCK_EX, false, fn)
}

// TryShared joins a namespace only if its shared lock is available immediately.
func TryShared(ctx context.Context, path string, fn func() error) error {
	return with(ctx, path, syscall.LOCK_SH, false, fn)
}

// Within acquires a lock below an explicitly selected root using pinned,
// identity-bound ancestry. Permanent gate/pool files must never be unlinked.
func Within(ctx context.Context, root, path string, shared, wait bool, fn func() error) error {
	d, err := ownedfs.Open(ctx, root, filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := d.OpenFile(ctx, filepath.Base(path), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	return flock(ctx, f, path, how, wait, func() error {
		if err := d.Validate(ctx); err != nil {
			return err
		}
		// A cooperating opener must still hold the current permanent inode after
		// waiting; an externally replaced lock cannot establish exclusion.
		current, err := d.Stat(ctx, filepath.Base(path))
		if err != nil {
			return err
		}
		held, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(current, held) {
			return ownedfs.ErrScope
		}
		return fn()
	})
}

func with(ctx context.Context, path string, how int, wait bool, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("lock %s is not a regular file", filepath.Base(path))
	}
	return flock(ctx, f, path, how, wait, fn)
}

func flock(ctx context.Context, f *os.File, path string, how int, wait bool, fn func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if !wait {
			return ErrBusy
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock %s: %w", filepath.Base(path), ctx.Err())
		case <-time.After(retry):
		}
	}
}
