package e2e

import (
	"os/exec"
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

// A name names one window, or else one session, exactly: tmux alone would take
// a prefix of one, so a name whose window closed would reach another agent. An
// agent's names resolve in the tmux session of the pane its process tree runs
// in, whatever $TMUX_PANE it inherited; a person's in the session tmux takes
// as theirs.
func TestANameNamesOneWindowExactly(t *testing.T) {
	h := start(t)
	elsewhere := h.tmux("new-session", "-d", "-s", "solo", "-n", "uniq", "-P", "-F", "#{pane_id}", "sleep 600")
	window := func(args ...string) string {
		return h.tmux(append([]string{"new-window", "-d", "-t", "=main:", "-P", "-F", "#{pane_id}"}, append(args, "sleep 600")...)...)
	}
	uniq := window("-n", "uniq")
	window("-n", "dup")
	window("-n", "dup")
	other := window("-n", "other")
	// tmux 3.2a and 3.3a keep a newline in a name; newer releases refuse one.
	if out, err := exec.Command("tmux", "-L", h.name, "new-window", "-d", "-t", "=main:", "-n", "line1\nh-a", "sleep 600").CombinedOutput(); err != nil && !strings.Contains(string(out), "invalid window name") {
		t.Fatalf("a window named with a newline: %v %s", err, out)
	}
	semi := window("-n", `ends in \;`) // tmux takes the name as "ends in ;"
	h.tmux("new-session", "-d", "-s", "other", "sleep 600")
	h.tmux("new-session", "-d", "-s", "dup", "sleep 600") // two windows share its name
	out := filepath.Join(h.dir, "agent")
	agent := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out},
		"env", "TMUX_PANE="+elsewhere, filepath.Join(bin, "claude"))
	h.waitPane(agent, "idle", "process")
	h.tmux("move-window", "-s", agent, "-t", "=main:") // spawn opened it in the newest session
	h.tmux("rename-window", "-t", agent, "w-10")
	t.Setenv("TMUX", h.tmux("display-message", "-p", "#{socket_path}")+",0,0")
	t.Setenv("TMUX_PANE", h.tmux("display-message", "-p", "-t", "=main:0", "#{pane_id}"))

	if r := h.tmag("", "inspect", "w-10", "--json"); r.code != 0 || !strings.Contains(r.out, `"id":"`+agent+`"`) {
		t.Fatalf("inspect w-10: %d %s", r.code, r.out)
	}
	for name, pane := range map[string]string{"uniq": uniq, "other": other, "solo": elsewhere, "ends in ;": semi} {
		if r := h.tmag("", "inspect", name); r.code != 1 || !strings.Contains(r.out, "in pane "+name+" ("+pane+")") {
			t.Fatalf("inspect %s, a window's or session's name: %d %s", name, r.code, r.out)
		}
	}
	for _, prefix := range []string{"w-1", "un", "oth", "sol", "h-a"} {
		if r := h.tmag("", "inspect", prefix); r.code != 1 || !strings.Contains(r.out, "no window of your tmux session or session has that exact name") {
			t.Fatalf("inspect %s, a prefix: %d %s", prefix, r.code, r.out)
		}
	}
	r := h.tmag("", "send", "dup", "hello", "--json")
	if r.code != 1 || !strings.Contains(r.out, `"code":"ambiguous"`) || !strings.Contains(r.out, "dup names 2 windows of your tmux session") {
		t.Fatalf("send to a name two windows and a session share: %d %s", r.code, r.out)
	}

	typed(t, h, agent, sidA, "tmag inspect uniq")
	readEventually(t, out, "tool 1 tmag inspect: no such agent in pane uniq ("+uniq+")")
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
