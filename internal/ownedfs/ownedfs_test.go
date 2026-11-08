package ownedfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedScopeRefusesParentAliasAndReplacement(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "real-directory", true: "symlink"}[alias], func(t *testing.T) {
			root := t.TempDir()
			ctx := context.Background()
			d, err := Open(ctx, root, "sessions/claude", true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			path := filepath.Join(root, "sessions")
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			foreign := t.TempDir()
			replacement := path
			if alias {
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
				replacement = foreign
			}
			if err := os.MkdirAll(filepath.Join(replacement, "claude"), 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(replacement, "claude", "slot")
			if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := d.OpenFile(ctx, "slot", os.O_CREATE|os.O_WRONLY, 0600); !errors.Is(err, ErrScope) {
				t.Fatalf("pinned replaced scope = %v", err)
			}
			if next, err := Open(ctx, root, "sessions/claude", true); !errors.Is(err, ErrScope) {
				if next != nil {
					next.Close()
				}
				t.Fatalf("reopened replaced scope = %v", err)
			}
			if b, err := os.ReadFile(marker); err != nil || string(b) != "preserve" {
				t.Fatalf("replacement changed: %q %v", b, err)
			}
		})
	}
}

func TestSlotReservationCannotAdoptExistingUnknownObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "claude")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(path, ".tmag-tmp-s-00")
	if err := os.WriteFile(slot, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenSlots(ctx, root, "sessions/claude", ".tmag-tmp-s-", 256, true); !errors.Is(err, ErrScope) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("claimed unknown slot: %v", err)
	}
	if d, err := OpenSlots(ctx, root, "sessions/claude", ".tmag-tmp-s-", 256, false); !errors.Is(err, ErrScope) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("cleanup manufactured ownership: %v", err)
	}
	if b, err := os.ReadFile(slot); err != nil || string(b) != "unknown" {
		t.Fatalf("unknown changed: %q %v", b, err)
	}
}

func TestSamePinnedDirectoryRenameAndRefusedCrossScope(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, err := Open(ctx, root, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, root, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	f, err := a.OpenFile(ctx, "tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("complete"); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := a.Rename(ctx, "tmp", b, "record", st); !errors.Is(err, ErrScope) {
		t.Fatalf("cross-scope rename = %v", err)
	}
	if err := a.Rename(ctx, "tmp", a, "record", st); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "a", "record")); err != nil || string(data) != "complete" {
		t.Fatalf("published = %q %v", data, err)
	}
}
