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
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

func TestExplicitLockCutoverFromOwnerTerminal(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	if mode, err := coord.Current(ctx, h.state); err != nil || mode != coord.Compatibility {
		t.Fatalf("fresh realm mode = %s, %v", mode, err)
	}
	if r := h.tmag("", "prune", "--retire-legacy-locks", "--server", h.name); r.code != 2 {
		t.Fatalf("realm cutover accepted server subset: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "prune", "--retire-legacy-locks"); r.code != 0 || !strings.Contains(r.out, "Persistent lock pools active") {
		t.Fatalf("owner cutover = %d %s", r.code, r.out)
	}
	if mode, err := coord.Current(ctx, h.state); err != nil || mode != coord.Pool {
		t.Fatalf("cutover mode = %s, %v", mode, err)
	}
	// The fresh test realm has no legacy writers or launch paths. Its new
	// agent's hooks and ordinary control verbs operate through the pools.
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	if r := h.tmag("", "send", sidA[:8], "--wait", "--timeout", "10s", "pool control"); r.code != 0 {
		t.Fatalf("pooled control = %d %s", r.code, r.out)
	}
	if entries, err := os.ReadDir(filepath.Join(h.state, "locks")); err != nil || len(entries) != 0 {
		t.Fatalf("pool-only hooks/control created legacy files: %d %v", len(entries), err)
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
	if mode, err := coord.Current(context.Background(), h.state); err != nil || mode != coord.Compatibility {
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
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			r := h.tmag("", "prune")
			if level == "sessions/claude" {
				if r.code != 1 || strings.TrimSpace(r.out) != "tmag prune: "+ownedfs.ErrScope.Error() {
					t.Fatalf("ambiguous parent claimed successful complete repair: %+v", r)
				}
			} else if r.code != 0 {
				t.Fatalf("unknown child preservation: %+v", r)
			}
			after, err := os.Lstat(path)
			target, readErr := os.Readlink(path)
			if err != nil || readErr != nil || !os.SameFile(before, after) || target != foreign {
				t.Fatalf("prune changed alias binding: %v %v", err, readErr)
			}
			if manifest, err := os.ReadFile(filepath.Join(h.state, ".tmag-scope.json")); err == nil && strings.Contains(string(manifest), `"sessions/claude"`) && level == "sessions/claude" {
				t.Fatal("prune adopted an ambiguous directory binding")
			}
			for _, name := range []string{"s-00", ".tmag-tmp-s-00"} {
				if b, err := os.ReadFile(filepath.Join(foreign, name)); err != nil || string(b) != "preserve" {
					t.Fatalf("prune touched alias %s: %q %v", name, b, err)
				}
			}
		})
	}
}
