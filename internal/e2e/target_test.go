package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A person names an agent the way tmux names panes: tmux resolves the target
// on the caller's server, so the TARGET tmag list prints is one tmag takes.
func TestTmuxTargetsNameAgents(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	h.tmux("rename-window", "-t", pane, "reviewer")
	target := h.tmux("display-message", "-p", "-t", pane, "#{session_name}:#{window_index}.#{pane_index}")
	// The person's shell runs in this server.
	t.Setenv("TMUX", h.tmux("display-message", "-p", "#{socket_path}")+",0,0")

	if r := h.tmag("", "list"); r.code != 0 || !strings.Contains(r.out, target+" ") {
		t.Fatalf("list does not print %s: %d %s", target, r.code, r.out)
	}
	if r := h.tmag("", "send", target, "--json", "hello"); r.code != 0 || !strings.Contains(r.out, `"id":"`+pane+`"`) {
		t.Fatalf("send %s: %d %s", target, r.code, r.out)
	}
	readEventually(t, out, "hello")
	for _, name := range []string{"main:reviewer", "main:reviewer.0", pane, sidA[:8]} {
		if r := h.tmag("", "wait", name, "--json"); r.code != 0 || !strings.Contains(r.out, `"id":"`+sidA+`"`) {
			t.Fatalf("wait %s: %d %s", name, r.code, r.out)
		}
	}
	if r := h.tmag("", "wait", "main:nowhere", "--timeout", "2s"); r.code != 1 || !strings.Contains(r.out, "tmux finds no pane by that target") {
		t.Fatalf("wait on a target tmux does not know: %d %s", r.code, r.out)
	}
}

// An agent started in a pane a moment ago, by a person or by another agent
// with tmux, shows its session from its first hook: until then verbs are
// refused and say to wait, and a wait on the pane lasts until it shows it.
func TestWaitFollowsAPaneUntilItShowsASession(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_START_MS": "3000"}, filepath.Join(bin, "claude"))
	r := h.tmag("", "send", h.target(pane), "hello")
	if r.code != 3 || !strings.Contains(r.out, "refused (unbound)") || !strings.Contains(r.out, "next: an agent shows its session from its first hook: tmag wait "+h.target(pane)+" --for idle") {
		t.Fatalf("send before the first hook: %d %s", r.code, r.out)
	}
	r = h.tmag("", "wait", h.target(pane), "--for", "idle", "--json")
	if r.code != 0 || !strings.Contains(r.out, `"id":"`+sidA+`"`) || !strings.Contains(r.out, `"state":"idle"`) {
		t.Fatalf("wait on the pane: %d %s", r.code, r.out)
	}
}
