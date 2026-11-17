package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Purging a state directory removes all it holds but its lock files, which
// a process may hold meanwhile.
func TestRemoveStateKeepsLockFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sessions/claude-s1.json", "events.jsonl", "hook.log", "seq", "lock", "hook-log.lock", "notify-0.lock", "locks/p-00", "locks/s-ff"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeState(dir); err != nil {
		t.Fatal(err)
	}
	var left []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			left = append(left, rel)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"hook-log.lock", "lock", "locks/p-00", "locks/s-ff", "notify-0.lock"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	if err := removeState(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("a missing state directory: %v", err)
	}
}
