package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "lock")
	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := With(context.Background(), path, func() error {
				mu.Lock()
				inside++
				maxInside = max(maxInside, inside)
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d holders at once", maxInside)
	}
}

func TestDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = With(context.Background(), path, func() error { close(held); <-release; return nil })
	}()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := With(ctx, path, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestReleasedAfterPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	func() {
		defer func() { _ = recover() }()
		_ = With(context.Background(), path, func() error { panic("boom") })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := With(ctx, path, func() error { return nil }); err != nil {
		t.Fatalf("lock not released after panic: %v", err)
	}
}

func TestCanceledUncontendedLockDoesNotCreateNamespaceOrRunCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unused", "lock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := With(ctx, path, func() error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled lock created directory: %v", err)
	}
}

// Resources share a bounded pool of lock files: two keys with the same first
// byte wait for each other, classes never do, and no more than one file per
// class and byte ever exists.
func TestStripesBoundTheLockFiles(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a, b, c := [32]byte{7, 1}, [32]byte{7, 2}, [32]byte{8}
	err := Stripe(ctx, dir, "s", a, func() error {
		short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
		defer stop()
		if err := Stripe(short, dir, "s", b, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a key of the same stripe got in: %v", err)
		}
		if err := Stripe(ctx, dir, "s", c, func() error { return nil }); err != nil {
			t.Errorf("another stripe waited: %v", err)
		}
		return Stripe(ctx, dir, "p", a, func() error { return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 * Stripes {
		key := [32]byte{byte(i)}
		if err := Stripe(ctx, dir, "s", key, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(filepath.Join(dir, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != Stripes+1 {
		t.Fatalf("%d lock files, want %d", len(files), Stripes+1)
	}
}

// A held lock excludes others until it is closed, and a lock file replaced by
// something else fails instead of blocking.
func TestAcquire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "lease")
	held, err := Acquire(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(ctx, path, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("second holder: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(ctx, path, false)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again.Close()
}

// All holds every stripe of its class, so a Stripe of that class waits for
// it, and two of it, taking stripes in one order, never wait on each other.
func TestAllHoldsEveryStripe(t *testing.T) {
	dir := t.TempDir()
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- All(context.Background(), dir, "p", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	for _, b := range []byte{0x00, 0x7f, 0xff} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := Stripe(ctx, dir, "p", [32]byte{b}, func() error { return nil })
		cancel()
		if err == nil {
			t.Fatalf("stripe %02x was taken while All held it", b)
		}
	}
	if err := Stripe(context.Background(), dir, "s", [32]byte{}, func() error { return nil }); err != nil {
		t.Fatalf("another class waited for All: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errs <- All(ctx, dir, "p", func() error { return nil })
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("two Alls at once: %v", err)
		}
	}
}
