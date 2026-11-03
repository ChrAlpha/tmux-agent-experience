package lockfile

import (
	"context"
	"errors"
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
