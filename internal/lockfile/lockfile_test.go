package lockfile

import (
	"context"
	"errors"
	"fmt"
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

func TestSharedParticipantsExcludeExclusiveCutover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate")
	ctx := context.Background()
	err := Shared(ctx, path, func() error {
		return Shared(ctx, path, func() error {
			if err := Try(ctx, path, func() error { t.Fatal("exclusive bypassed shared holders"); return nil }); !errors.Is(err, ErrBusy) {
				return fmt.Errorf("exclusive attempt: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Try(ctx, path, func() error { return nil }); err != nil {
		t.Fatalf("released shared holders still exclude writer: %v", err)
	}
}
