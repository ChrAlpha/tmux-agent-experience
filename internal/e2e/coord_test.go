package e2e

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

func TestPrunePreservesUnknownForeignRealmsEvenWithAgedHeldLocks(t *testing.T) {
	h := start(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(h.dir, "xdg-state"))
	h.state = filepath.Join(eventlog.Base(), "aaaaaaaaaaaa")
	t.Setenv("TMAG_STATE_DIR", h.state)
	foreign := filepath.Join(eventlog.Base(), "bbbbbbbbbbbb")
	paths := []string{
		filepath.Join(foreign, "lock"),
		filepath.Join(foreign, "sessions", "codex", "unknown.json"),
		filepath.Join(eventlog.Base(), "events.jsonl"),
		filepath.Join(eventlog.Base(), "locks", "s-0123456789abcdef"),
		filepath.Join(eventlog.Base(), "auth", "metadata-sentinel"),
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	held, err := os.OpenFile(paths[0], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if r := h.tmag("", "prune"); r.code != 0 {
		t.Fatalf("prune = %d %s", r.code, r.out)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("automatic prune destroyed unknown metadata %s: %v", path, err)
		}
	}
}

// Unknown child directories may alias unrelated same-owner metadata. Exercise
// preservation of both child aliases and pinned ancestors through the real
// command, including fixed-looking names that convey no ownership proof.
func TestPrunePreservesAliasedSessionTemporaryParents(t *testing.T) {
	for _, level := range []string{"sessions/claude/.tmag-tmp", "sessions/claude"} {
		t.Run(level, func(t *testing.T) {
			h := start(t)
			path := filepath.Join(h.state, level)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			foreign := t.TempDir()
			if err := os.Symlink(foreign, path); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"s-00", ".tmag-tmp-s-00"} {
				if err := os.WriteFile(filepath.Join(foreign, name), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_ = h.tmag("", "prune") // A refused alias must remain untouched.
			for _, name := range []string{"s-00", ".tmag-tmp-s-00"} {
				if b, err := os.ReadFile(filepath.Join(foreign, name)); err != nil || string(b) != "preserve" {
					t.Fatalf("prune touched alias %s: %q %v", name, b, err)
				}
			}
		})
	}
}
