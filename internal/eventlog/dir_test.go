package eventlog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

func TestDefaultDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv("TMAG_STATE_DIR", "")
	if got, want := DefaultDir(), filepath.Join(xdg, "tmag", proc.Realm()); got != want {
		t.Errorf("DefaultDir() = %q, want the realm's directory %q", got, want)
	}
	t.Setenv("TMAG_STATE_DIR", "/scratch/state")
	if got := DefaultDir(); got != "/scratch/state" {
		t.Errorf("DefaultDir() = %q, want $TMAG_STATE_DIR as it is", got)
	}
}

func TestPruneStale(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := Base()
	old := time.Now().Add(-2 * StaleAge)
	cutoff := time.Now().Add(-StaleAge)
	write := func(path string, mtime time.Time) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	own := filepath.Join(base, "aaaaaaaaaaaa")
	write(filepath.Join(own, "events.jsonl"), old) // never pruned, however old
	gone := filepath.Join(base, "bbbbbbbbbbbb")
	write(filepath.Join(gone, "sessions", "claude", "x.json"), old)
	idle := filepath.Join(base, "cccccccccccc")
	write(filepath.Join(idle, "events.jsonl"), old)
	write(filepath.Join(idle, "sessions", "codex", "y.json"), time.Now())
	write(filepath.Join(base, "events.jsonl"), old)
	write(filepath.Join(base, "sessions", "claude", "z.json"), old)
	write(filepath.Join(base, "notes.txt"), old) // not tmag's

	if removed, err := PruneStale(filepath.Join(t.TempDir(), "state"), cutoff); err != nil || len(removed) > 0 {
		t.Fatalf("a directory outside Base pruned %v, %v", removed, err)
	}
	if removed, err := PruneStale(base, cutoff); err != nil || len(removed) > 0 {
		t.Fatalf("Base itself, the state of an unknown realm, pruned %v, %v", removed, err)
	}
	removed, err := PruneStale(own, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{gone, filepath.Join(base, "events.jsonl"), filepath.Join(base, "sessions")}
	slices.Sort(removed)
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for _, p := range []string{own, idle, filepath.Join(base, "notes.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v, want it kept", p, err)
		}
	}

	// Legacy state is one state: a part written lately keeps all of it.
	write(filepath.Join(base, "events.jsonl"), old)
	write(filepath.Join(base, "seq"), time.Now())
	if removed, err := PruneStale(own, cutoff); err != nil || len(removed) > 0 {
		t.Errorf("legacy state in use pruned %v, %v", removed, err)
	}
}
