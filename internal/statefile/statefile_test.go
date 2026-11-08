package statefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicPublicationAndCanceledReplacement(t *testing.T) {
	dir := t.TempDir()
	path, tmp := filepath.Join(dir, "record"), filepath.Join(dir, "slot")
	if err := Write(context.Background(), path, tmp, []byte("old")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Write(ctx, path, tmp, []byte("new")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled replacement = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "old" {
		t.Fatalf("canceled replacement changed target: %q, %v", got, err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled replacement created temp: %v", err)
	}
	if err := Write(context.Background(), path, tmp, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("replacement = %q, %v", got, err)
	}
}

func TestUncertainAliasesAreNeitherWrittenNorRemoved(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink", true: "hardlink"}[hard], func(t *testing.T) {
			dir := t.TempDir()
			other, tmp, target := filepath.Join(dir, "unrelated"), filepath.Join(dir, "slot"), filepath.Join(dir, "record")
			if err := os.WriteFile(other, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if hard {
				link = os.Link
			}
			if err := link(other, tmp); err != nil {
				t.Fatal(err)
			}
			if err := Write(context.Background(), target, tmp, []byte("overwrite")); err == nil {
				t.Fatal("wrote through an uncertain alias")
			}
			if removed, err := Remove(context.Background(), tmp); err != nil || removed {
				t.Fatalf("removed uncertain alias: %v, %v", removed, err)
			}
			if got, err := os.ReadFile(other); err != nil || string(got) != "preserve" {
				t.Fatalf("unrelated file changed: %q, %v", got, err)
			}
		})
	}
}

func TestSmallReadBoundsMetadataAndCleanupPreservesTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slot")
	if err := os.WriteFile(path, []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSmall(context.Background(), path, 8); err == nil {
		t.Fatal("read oversized metadata")
	}
	if removed, err := Remove(context.Background(), path); err != nil || !removed {
		t.Fatalf("owned temporary removal = %v, %v", removed, err)
	}
}
