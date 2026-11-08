package ownedfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenKnownNeverAdoptsOrCreatesBindings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, err := Open(ctx, root, "known", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ".tmag-scope.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	known, err := OpenKnown(ctx, root, "known")
	if err != nil {
		t.Fatal(err)
	}
	if err := known.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "unregistered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenKnown(ctx, root, "unregistered"); d != nil || !errors.Is(err, ErrScope) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("reader adopted unknown binding: %v", err)
	}
	if d, err := OpenKnown(ctx, root, "missing"); d != nil || !errors.Is(err, os.ErrNotExist) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("reader created missing directory: %v", err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("diagnostic read rewrote ownership evidence", err)
	}
	if err := os.Rename(filepath.Join(root, "known"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "known"), 0o700); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenKnown(ctx, root, "known"); d != nil || !errors.Is(err, ErrScope) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("replaced binding accepted: %v", err)
	}
}
