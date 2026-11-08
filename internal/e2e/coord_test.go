package e2e

import (
	"context"
	"errors"
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
	if _, err := os.Stat(filepath.Join(h.state, "locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pool-only hooks/control created legacy files: %v", err)
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
