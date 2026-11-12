// Package lockfile provides advisory file locks whose waiting is bounded by a
// context.
//
// Lock files are never removed, so a lock held through an open file always
// guards the name others open.
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

// ErrBusy is returned by Acquire without waiting when another holder
// excludes this one.
var ErrBusy = errors.New("lock busy")

// With runs fn while holding an exclusive flock on path, creating the file and
// its directory if needed. Waiting for the lock ends with ctx. Closing the file
// releases the lock, so it is released even if fn panics.
func With(ctx context.Context, path string, fn func() error) error {
	f, err := open(ctx, path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := flock(ctx, f, path, syscall.LOCK_EX, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// Stripes is how many locks a pool of Stripe holds per class.
const Stripes = 256

// Stripe runs fn holding the lock of key in a fixed pool under dir/locks:
// one file per class and first byte of key, so that the number of lock files
// stays bounded however many resources come and go. Resources that share a
// stripe wait for each other. Whoever takes stripes of two classes takes
// them in a fixed order, and nobody takes two stripes of one class.
func Stripe(ctx context.Context, dir, class string, key [32]byte, fn func() error) error {
	return With(ctx, filepath.Join(dir, "locks", fmt.Sprintf("%s-%02x", class, key[0])), fn)
}

// Held is a lock held until Close, for a holder whose lifetime is not one
// function call: a lease a child process inherits.
type Held struct{ file *os.File }

// File is the locked descriptor, for a child to inherit; Held keeps owning it.
func (h *Held) File() *os.File { return h.file }

// Close releases the lock.
func (h *Held) Close() error { return h.file.Close() }

// Acquire takes the exclusive lock on path and returns it held, waiting for
// it until ctx ends when wait is true, and failing with ErrBusy otherwise.
func Acquire(ctx context.Context, path string, wait bool) (*Held, error) {
	f, err := open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := flock(ctx, f, path, syscall.LOCK_EX, wait); err != nil {
		f.Close()
		return nil, err
	}
	return &Held{file: f}, nil
}

func open(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func flock(ctx context.Context, f *os.File, path string, how int, wait bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return nil
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
