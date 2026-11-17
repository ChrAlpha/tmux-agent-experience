package hook

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/sockettest"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestGuardPredicates(t *testing.T) {
	srv, pane := startTmuxPane(t)
	ctx := context.Background()
	set := func(agent, id, pid, start, state string) {
		t.Helper()
		if err := srv.SetPaneOptions(ctx, pane, map[string]string{
			"agent": agent, "session": id, "pid": pid, "start": start, "state": state,
			"kind": "old", "since": "111",
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	hint := session.PaneHint{Server: srv.Socket, ID: pane, PID: 123, Start: "12345"}
	projection := func() string {
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_kind}|#{@tmag_since}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	// The right pane accepts and writes the projection.
	set("claude", "session", "123", "12345", "idle")
	if _, _, _, _, _, ok := writeHintProjection(ctx, hint, "claude", "session", attach.Projection{State: "working"}, false); !ok || projection() != "working||" {
		t.Fatalf("the full guard refused: projection=%q", projection())
	}

	// Each predicate alone must refuse without touching the projection.
	for _, tc := range []struct {
		name                  string
		agent, id, pid, start string
	}{
		{"foreign agent", "codex", "session", "123", "12345"},
		{"foreign session", "claude", "other", "123", "12345"},
		{"other pid", "claude", "session", "999", "12345"},
		{"other start", "claude", "session", "123", "99999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set(tc.agent, tc.id, tc.pid, tc.start, "idle")
			if _, _, _, _, _, ok := writeHintProjection(ctx, hint, "claude", "session", attach.Projection{State: "working"}, false); ok {
				t.Fatal("the guard accepted a false predicate")
			}
			if got := projection(); got != "idle|old|111" {
				t.Fatalf("projection changed on refusal: %q", got)
			}
		})
	}

	// A dead pane refuses too, even when every option still matches.
	set("claude", "session", "123", "12345", "idle")
	if _, err := srv.Run(ctx, "set-window-option", "-t", "main:0", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Run(ctx, "respawn-pane", "-k", "-t", pane, "/bin/true"); err != nil {
		t.Fatal(err)
	}
	dead := ""
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{pane_dead}")
		if err != nil {
			t.Fatal(err)
		}
		dead = strings.TrimSpace(string(out))
		if dead == "1" {
			break
		}
	}
	if dead != "1" {
		t.Fatalf("pane is not dead: %q", dead)
	}
	if _, _, _, _, _, ok := writeHintProjection(ctx, hint, "claude", "session", attach.Projection{State: "working"}, false); ok {
		t.Fatal("the guard accepted a dead pane")
	}
	if got := projection(); got != "idle|old|111" {
		t.Fatalf("projection changed on a dead pane: %q", got)
	}
}

// startTmuxPane starts a private server and returns a pane in it.
func startTmuxPane(t *testing.T) (tmux.Server, string) {
	t.Helper()
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("TMUX_TMPDIR", sockettest.Dir(t))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	name := "guard"
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(tmuxPath, append([]string{"-L", name}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh", "-c", "sleep 60")
	t.Cleanup(func() {
		_ = exec.Command(tmuxPath, "-L", name, "kill-server").Run()
	})
	socket := run("display-message", "-p", "#{socket_path}")
	pane := run("list-panes", "-a", "-F", "#{pane_id}")
	return tmux.Server{Socket: socket}, pane
}
