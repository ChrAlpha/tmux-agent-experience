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
)

// retry is how often a contended lock is tried again.
const retry = 5 * time.Millisecond

// With runs fn while holding an exclusive flock on path, creating the file and
// its directory if needed. Waiting for the lock ends with ctx. Closing the file
// releases the lock, so it is released even if fn panics.
func With(ctx context.Context, path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock %s: %w", filepath.Base(path), ctx.Err())
		case <-time.After(retry):
		}
	}
}
