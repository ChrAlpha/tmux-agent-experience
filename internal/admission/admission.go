// Package admission bounds cooperating processes with permanent flock slots.
// A lease is live until its last inherited descriptor closes; no expiry or PID
// guess may reclaim it, and no maintenance operation unlinks a slot.
package admission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
)

type Class string

const (
	Watch  Class = "w"
	Notify Class = "n"
	Slots        = 256
)

var ErrLimit = errors.New("resource_limit")
var ErrUnavailable = errors.New("admission_unavailable")

type Lease struct {
	file  *os.File
	dir   *ownedfs.Dir
	root  string
	class Class
	slot  int
	once  sync.Once
	err   error
}

func (l *Lease) File() *os.File { return l.file }
func (l *Lease) Slot() int      { return l.slot }
func (l *Lease) Close() error {
	l.once.Do(func() { l.err = errors.Join(l.file.Close(), l.dir.Close()) })
	return l.err
}

func spec(class Class) (policy.Spec, error) {
	switch class {
	case Watch:
		return policy.Watch, nil
	case Notify:
		return policy.Notify, nil
	}
	return policy.Spec{}, ErrUnavailable
}
func Acquire(ctx context.Context, root string, class Class) (*Lease, error) {
	return acquire(ctx, root, class, policy.Resolve)
}

type resolver func(context.Context, policy.Spec) (int, error)

func acquire(ctx context.Context, root string, class Class, resolve resolver) (*Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	option, err := spec(class)
	if err != nil {
		return nil, err
	}
	var lease *Lease
	// Gate contention is not evidence of full capacity. Wait only inside this
	// acquisition's existing 100ms budget; timeout is unavailable, never limit.
	err = lockfile.Within(ctx, root, "coord/admission-lock", false, true, func() error {
		limit, err := resolve(ctx, option)
		if err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		if limit == 0 {
			return ErrLimit
		}
		if limit < 0 || limit > Slots {
			return ErrUnavailable
		}
		d, err := ownedfs.OpenSlots(ctx, root, "coord", string(class)+"-", Slots, true)
		if err != nil {
			return err
		}
		keep := false
		defer func() {
			if !keep {
				_ = d.Close()
			}
		}()
		occupied, free := 0, -1
		for slot := range Slots {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := fmt.Sprintf("%s-%02x", class, slot)
			f, err := d.OpenFile(ctx, name, os.O_RDWR, 0)
			if errors.Is(err, os.ErrNotExist) {
				if free < 0 {
					free = slot
				}
				continue
			}
			if err != nil {
				return err
			}
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			_ = f.Close() // never LOCK_UN: inherited descriptions retain their lock
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				occupied++
				continue
			}
			if err != nil {
				return err
			}
			if free < 0 {
				free = slot
			}
		}
		if occupied >= limit || free < 0 {
			return ErrLimit
		}
		f, err := d.OpenFile(ctx, fmt.Sprintf("%s-%02x", class, free), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			return err
		}
		lease = &Lease{file: f, dir: d, root: root, class: class, slot: free}
		keep = true
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLimit) {
			return nil, err
		}
		return nil, errors.Join(ErrUnavailable, err)
	}
	return lease, nil
}

// InheritFD borrows an untrusted candidate number. Duplicate it before any
// filesystem discovery can reuse an absent descriptor. Only the private copy
// is owned or closed here, on both success and failure; the caller retains its
// original even when the environment names an unrelated runtime descriptor.
func InheritFD(ctx context.Context, root string, class Class, slot, candidate int) (*Lease, error) {
	if ctx.Err() != nil {
		return nil, errors.Join(ErrUnavailable, ctx.Err())
	}
	if _, err := spec(class); err != nil || slot < 0 || slot >= Slots || candidate < 0 {
		return nil, ErrUnavailable
	}
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(candidate)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, ErrUnavailable
	}
	return inheritOwned(ctx, root, class, slot, os.NewFile(uintptr(fd), "borrowed admission"))
}

// inheritOwned consumes only an already-owned descriptor. The inode and lock
// proof precedes admission; neither a candidate FD nor environment is a lease.
func inheritOwned(ctx context.Context, root string, class Class, slot int, f *os.File) (_ *Lease, err error) {
	if f == nil {
		return nil, ErrUnavailable
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	if _, err := spec(class); err != nil || slot < 0 || slot >= Slots {
		return nil, ErrUnavailable
	}
	d, err := ownedfs.OpenSlots(ctx, root, "coord", string(class)+"-", Slots, false)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() {
		if !keep {
			_ = d.Close()
		}
	}()
	probe, err := d.OpenFile(ctx, fmt.Sprintf("%s-%02x", class, slot), os.O_RDWR, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer probe.Close()
	held, err := f.Stat()
	if err != nil {
		return nil, ErrUnavailable
	}
	expected, err := probe.Stat()
	if err != nil || !os.SameFile(held, expected) {
		return nil, ErrUnavailable
	}
	// Validate the inherited description actually owns the lock. A fresh
	// descriptor must conflict; taking the lock on the inherited one must work.
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		return nil, ErrUnavailable
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, ErrUnavailable
	}
	keep = true
	return &Lease{file: f, dir: d, root: root, class: class, slot: slot}, nil
}

type contextKey struct{}

// WithLease is internal typed borrowing for the in-process SSE CLI runner.
func WithLease(ctx context.Context, l *Lease) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}
func Borrow(ctx context.Context, root string, class Class) bool {
	l, ok := ctx.Value(contextKey{}).(*Lease)
	if !ok || l == nil || l.root != root || l.class != class {
		return false
	}
	_, err := l.file.Stat()
	return err == nil
}
