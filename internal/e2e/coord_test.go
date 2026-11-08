package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

func TestExplicitLockCutoverFromOwnerTerminal(t *testing.T) {
	h := start(t)
	// This fixture is old state: no writers run until the person migrates it.
	legacy := filepath.Join(h.state, "locks", "s-0000000000000000")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, nil, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if r := h.tmag("", "list"); r.code == 0 || !strings.Contains(r.out, "retire-legacy-locks") {
		t.Fatalf("legacy ordinary invocation = %d %s", r.code, r.out)
	}
	if r := h.tmag("", "prune", "--retire-legacy-locks", "--server", h.name); r.code != 2 {
		t.Fatalf("migration accepted server subset: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "prune", "--retire-legacy-locks"); r.code != 0 || !strings.Contains(r.out, "Persistent lock pools active") {
		t.Fatalf("owner migration = %d %s", r.code, r.out)
	}
	if mode, err := coord.Current(context.Background(), h.state); err != nil || mode != coord.Pool {
		t.Fatalf("migration mode = %s %v", mode, err)
	}
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	if r := h.tmag("", "send", sidA[:8], "--wait", "--timeout", "10s", "pool control"); r.code != 0 {
		t.Fatalf("pooled control = %d %s", r.code, r.out)
	}
	after, err := os.Stat(legacy)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("migration rebound legacy lock: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(legacy)); err != nil || len(entries) != 1 {
		t.Fatalf("new identifiers created legacy locks: %d %v", len(entries), err)
	}
}

func TestAgentCannotDeclareLegacyRetirement(t *testing.T) {
	h := start(t)
	d := h.daemon()
	d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": sidC, "source": "startup"})
	h.waitSession(sidC, "idle")
	r := d.tool(sidC, "prune", "--retire-legacy-locks")
	if r.code != 3 || !strings.Contains(r.out, "refused (agent)") {
		t.Fatalf("agent declared retirement: %d %s", r.code, r.out)
	}
	if mode, err := coord.Current(context.Background(), h.state); err != nil || mode != coord.Pool {
		t.Fatalf("refused declaration changed mode: %s, %v", mode, err)
	}
}

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
			if _, err := coord.Current(context.Background(), h.state); err != nil {
				t.Fatal(err)
			}
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
