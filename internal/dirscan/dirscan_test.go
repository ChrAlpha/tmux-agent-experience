package dirscan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

func TestNativePageCursorSurvivesReopenAndUnknownPrefix(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	d, err := ownedfs.Open(ctx, root, "entries", true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	expected := map[string]bool{}
	for i := range 1024 {
		name := fmt.Sprintf("unknown\\%04d", i)
		expected[name] = true
		if err := os.WriteFile(filepath.Join(root, "entries", name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var cursor Cursor
	seen := map[string]bool{}
	for pass := 0; pass < 512 && len(seen) < len(expected); pass++ {
		d, err := ownedfs.Open(ctx, root, "entries", false)
		if err != nil {
			t.Fatal(err)
		}
		names, next, examined, err := Next(ctx, d, cursor, 7)
		d.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(names) > 7 || examined > MaxNames || !next.Valid() {
			t.Fatalf("scan exceeded bound: %d %d %+v", len(names), examined, next)
		}
		for _, name := range names {
			if !expected[name] {
				t.Fatalf("unexpected name %q", name)
			}
			seen[name] = true
		}
		cursor = next
	}
	if len(seen) != len(expected) {
		t.Fatalf("reopened native cursor saw %d/%d entries", len(seen), len(expected))
	}
}

func TestCanceledNativePageAndUnknownNames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := ownedfs.Open(context.Background(), t.TempDir(), "entries", true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, _, _, err := Next(ctx, d, Cursor{}, 1); err == nil {
		t.Fatal("canceled scan succeeded")
	}
	if (Cursor{Pending: []string{"../foreign"}}).Valid() {
		t.Fatal("accepted an escaping saved name")
	}
}
