package coord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/dirscan"
)

func TestRetiredLockCleanupPreservesCompatibilityAndProgressesPastUnknowns(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	path := filepath.Join(root, "locks")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 1024 {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("unknown-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	known := []string{}
	for i := range 80 {
		name := fmt.Sprintf("s-%016x", i)
		known = append(known, name)
		if err := os.WriteFile(filepath.Join(path, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(filepath.Join(path, known[0]))
	if err != nil {
		t.Fatal(err)
	}
	// Pre-opened old waiter is preserved in compatibility even before locking.
	waiter, err := os.OpenFile(filepath.Join(path, known[0]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := CleanupRetired(ctx, root); err != nil || r.Removed != 0 {
		t.Fatalf("compat cleanup = %+v %v", r, err)
	}
	after, err := os.Stat(filepath.Join(path, known[0]))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("compatibility replaced a pre-opened legacy inode")
	}
	waiter.Close() // satisfy explicit retirement preconditions, not an inference
	if err := RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(filepath.Join(path, known[0]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	removed := 0
	for range 256 {
		r, err := CleanupRetired(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if r.Examined > dirscan.MaxNames || r.Candidates > 32 {
			t.Fatalf("scan exceeded bound: %+v", r)
		}
		removed += r.Removed
		if removed == len(known)-1 {
			break
		}
	}
	if removed != len(known)-1 {
		t.Fatalf("retired cleanup made insufficient progress: %d/%d", removed, len(known)-1)
	}
	if _, err := os.Stat(filepath.Join(path, known[0])); err != nil {
		t.Fatal("removed contended inode")
	}
	for i := range 1024 {
		if _, err := os.Stat(filepath.Join(path, fmt.Sprintf("unknown-%04d", i))); err != nil {
			t.Fatal("removed unknown object")
		}
	}
	held.Close()
	for range 256 {
		if _, err := CleanupRetired(ctx, root); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(path, known[0])); errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	t.Fatal("released legacy lock was never reclaimed")
}

func TestRetirementBindsLegacyParentsBeforeReclamation(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if err := RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "locks")
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "s-0000000000000000")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CleanupRetired(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("adopted a replacement legacy directory: %v", err)
	}
}
