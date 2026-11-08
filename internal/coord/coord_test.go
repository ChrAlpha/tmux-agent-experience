package coord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
)

func openLegacy(t *testing.T, dir string, class Class, key [32]byte) *os.File {
	t.Helper()
	path := legacyPath(dir, class, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func rawLock(t *testing.T, f *os.File, how int) {
	t.Helper()
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func TestFreshDirectoryRequiresExplicitRetirement(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	mode, err := Current(ctx, dir)
	if err != nil || mode != Compatibility {
		t.Fatalf("fresh mode = %s, %v", mode, err)
	}
	var key [32]byte
	if err := With(ctx, dir, Session, key, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath(dir, Session, key)); err != nil {
		t.Fatalf("fresh compatibility skipped legacy coordination: %v", err)
	}
	// A free lock and an otherwise empty namespace are never a cutover signal.
	if mode, err := Current(ctx, dir); err != nil || mode != Compatibility {
		t.Fatalf("implicitly retired old binaries: %s, %v", mode, err)
	}
}

func TestCompatibilityPreservesHolderAndPreopenedWaiterInode(t *testing.T) {
	dir := t.TempDir()
	var key [32]byte
	holder := openLegacy(t, dir, Session, key)
	waiter := openLegacy(t, dir, Session, key)
	before, err := waiter.Stat()
	if err != nil {
		t.Fatal(err)
	}
	rawLock(t, holder, syscall.LOCK_EX)
	tryNew := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := With(ctx, dir, Session, key, func() error {
			t.Fatal("new writer bypassed a legacy holder")
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("mixed-version exclusion = %v", err)
		}
	}
	tryNew()
	rawLock(t, holder, syscall.LOCK_UN)
	// This descriptor opened before either new attempt. Merely acquiring the
	// old path between holders cannot justify rebinding its inode.
	rawLock(t, waiter, syscall.LOCK_EX)
	tryNew()
	after, err := os.Stat(legacyPath(dir, Session, key))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("compatibility rebound a pre-opened inode: %v", err)
	}
	rawLock(t, waiter, syscall.LOCK_UN)
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waiter.Close(); err != nil {
		t.Fatal(err)
	}
	// Only now do the test's operator preconditions hold.
	if err := RetireLegacy(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := With(context.Background(), dir, Session, key, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err = os.Stat(legacyPath(dir, Session, key))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("cutover unnecessarily removed legacy evidence: %v", err)
	}
}

func TestCutoverExcludesNewWaitersAndHolders(t *testing.T) {
	dir := t.TempDir()
	var key [32]byte
	old := openLegacy(t, dir, Pane, key)
	rawLock(t, old, syscall.LOCK_EX)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- With(ctx, dir, Pane, key, func() error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	// The stripe is held before the new participant opens/waits on legacy.
	for {
		err := lockfile.Try(ctx, poolPath(dir, Pane, key), func() error { return nil })
		if errors.Is(err, lockfile.ErrBusy) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("new participant never acquired its stripe")
		case <-time.After(time.Millisecond):
		}
	}
	cutoverMustWait := func() {
		t.Helper()
		quick, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		defer stop()
		if err := RetireLegacy(quick, dir); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cutover bypassed participant: %v", err)
		}
		if mode, err := Current(ctx, dir); err != nil || mode != Compatibility {
			t.Fatalf("cutover published before participant drained: %s, %v", mode, err)
		}
	}
	cutoverMustWait()
	rawLock(t, old, syscall.LOCK_UN)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("new participant never entered callback")
	}
	cutoverMustWait()
	// Drain the callback without closing release twice in the cleanup.
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := RetireLegacy(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}

func TestPoolBoundAndStableInodesAfterExplicitCutover(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := RetireLegacy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	gateBefore, err := os.Stat(Gate(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1024 {
		key := [32]byte{byte(i), byte(i >> 8)}
		for _, class := range []Class{Pane, Session} {
			if err := With(ctx, dir, class, key, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(root(dir))
	if err != nil || len(entries) != 2*Slots+2 {
		t.Fatalf("coordination files = %d, %v; want %d", len(entries), err, 2*Slots+2)
	}
	if _, err := os.Stat(filepath.Join(dir, "locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pool-only mode created per-ID files: %v", err)
	}
	if err := RetireLegacy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	gateAfter, err := os.Stat(Gate(dir))
	if err != nil || !os.SameFile(gateBefore, gateAfter) {
		t.Fatalf("idempotent cutover rebound gate: %v", err)
	}
}

func TestClassesRemainSeparateAndPoolCollisionsSerialize(t *testing.T) {
	dir := t.TempDir()
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := RetireLegacy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	a, b := [32]byte{7, 1}, [32]byte{7, 2}
	err := With(ctx, dir, Pane, a, func() error {
		// Identical stripe number in another class must remain usable while
		// preserving the pane-before-session order.
		if err := With(ctx, dir, Session, a, func() error { return nil }); err != nil {
			return err
		}
		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		err := With(short, dir, Pane, b, func() error { return errors.New("collision bypassed stripe") })
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("same-class collision: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnknownModeCannotSelectAnotherCriticalSection(t *testing.T) {
	dir := t.TempDir()
	if _, err := Current(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root(dir), "mode"), []byte("unknown-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := With(context.Background(), dir, Session, [32]byte{}, func() error {
		t.Fatal("unknown mode entered a critical section")
		return nil
	})
	if err == nil {
		t.Fatal("unknown mode silently selected compatibility")
	}
}

// QA can compare the fixed pool's contention/overhead with dual coordination
// using the same bounded working set; the benchmark never touches live state.
func BenchmarkPoolCoordination(b *testing.B) {
	for _, pooled := range []bool{false, true} {
		name := "compatibility"
		if pooled {
			name = "pool"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			ctx := context.Background()
			if pooled {
				if err := RetireLegacy(ctx, dir); err != nil {
					b.Fatal(err)
				}
			}
			var next atomic.Uint64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := next.Add(1) % 4096
					key := [32]byte{byte(i), byte(i >> 8)}
					if err := With(ctx, dir, Session, key, func() error { return nil }); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
