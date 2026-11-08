package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingProbeIsNonblockingAndDoesNotCreate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	holder, err := AcquireWithin(ctx, root, "observe/refresh-lock", false, false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, ".tmag-scope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if probe, err := TryExisting(ctx, root, "observe/refresh-lock"); probe != nil || !errors.Is(err, ErrBusy) {
		if probe != nil {
			_ = probe.Close()
		}
		t.Fatalf("probe waited or ignored holder: %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	probe, err := TryExisting(ctx, root, "observe/refresh-lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if p, err := TryExisting(ctx, root, "observe/missing-lock"); p != nil || !errors.Is(err, os.ErrNotExist) {
		if p != nil {
			_ = p.Close()
		}
		t.Fatalf("missing probe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "observe", "missing-lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostic created lock", err)
	}
	after, err := os.ReadFile(filepath.Join(root, ".tmag-scope.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("probe changed manifest", err)
	}
}
