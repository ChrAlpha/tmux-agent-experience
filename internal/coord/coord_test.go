package coord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestFreshDirectoryUsesFixedPools(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mode, err := Current(ctx, dir); err != nil || mode != Pool {
				t.Errorf("initialize = %s %v", mode, err)
			}
		}()
	}
	wg.Wait()
	before, err := os.Stat(Gate(dir))
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
		t.Fatalf("coordination objects = %d %v", len(entries), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created legacy locks: %v", err)
	}
	if err := RetireLegacy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(Gate(dir))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("rebound gate: %v", err)
	}
}

func TestLegacyStateRequiresExplicitMigrationAndPreservesLockInodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks", "s-0000000000000000")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	waiter, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	before, err := holder.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	err = With(context.Background(), dir, Session, [32]byte{}, func() error { t.Fatal("legacy state entered callback"); return nil })
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("legacy admission = %v", err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	// Closing/quiescing is the operator's responsibility. No automatic migration
	// is inferred from the now-free inode, even with a pre-opened waiter.
	if _, err := Current(context.Background(), dir); !errors.Is(err, ErrMigration) {
		t.Fatalf("free lock triggered migration: %v", err)
	}
	if err := RetireLegacy(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := With(context.Background(), dir, Session, [32]byte{}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("migration removed a legacy inode: %v", err)
	}
	if err := syscall.Flock(int(waiter.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("pre-opened descriptor changed: %v", err)
	}
}

func TestClassesRemainSeparateAndPoolCollisionsSerialize(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	a, b := [32]byte{7, 1}, [32]byte{7, 2}
	err := With(ctx, dir, Pane, a, func() error {
		if err := With(ctx, dir, Session, a, func() error { return nil }); err != nil {
			return err
		}
		quick, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		err := With(quick, dir, Pane, b, func() error { return errors.New("collision bypassed stripe") })
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("collision: %w", err)
		}
		migration, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		defer stop()
		if err := RetireLegacy(migration, dir); !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("migration bypassed participant: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnknownStateAndAliasesRefuse(t *testing.T) {
	for _, kind := range []string{"legacy-mode", "unknown-mode", "unknown-root", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			foreign := t.TempDir()
			switch kind {
			case "legacy-mode", "unknown-mode":
				if _, err := Current(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
				mode := "pool-v1\n"
				if kind == "unknown-mode" {
					mode = "unknown\n"
				}
				if err := os.WriteFile(filepath.Join(root(dir), "mode"), []byte(mode), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-root":
				if err := os.WriteFile(filepath.Join(dir, "unrelated"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(foreign, root(dir)); err != nil {
					t.Fatal(err)
				}
			}
			entered := false
			if err := With(context.Background(), dir, Session, [32]byte{}, func() error { entered = true; return nil }); err == nil || entered {
				t.Fatalf("unsupported state admitted: %v", err)
			}
			if kind == "legacy-mode" {
				if err := RetireLegacy(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "unknown-mode" {
				if err := RetireLegacy(context.Background(), dir); err == nil {
					t.Fatal("unknown format migrated")
				}
			}
			if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
				t.Fatalf("alias target changed: %v", err)
			}
		})
	}
}
