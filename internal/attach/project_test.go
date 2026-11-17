package attach

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/sockettest"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// startTmuxPane starts a private server and returns a pane in it and the
// program it runs.
func startTmuxPane(t *testing.T) (tmux.Server, string, string) {
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
	var cmd string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cmd = run("display-message", "-p", "-t", pane, "#{pane_current_command}"); cmd == "sleep" {
			break
		}
	}
	return tmux.Server{Socket: socket}, pane, cmd
}

func TestGuardPredicates(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	set := func(agent, id, pid, start, command, state string) {
		t.Helper()
		if err := srv.SetPaneOptions(ctx, pane, map[string]string{
			"agent": agent, "session": id, "pid": pid, "start": start, "evidence": Process, "cmd": command,
			"state": state, "kind": "old", "since": "111",
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	entry := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	projection := func() string {
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_kind}|#{@tmag_since}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	// The right pane accepts and writes the projection.
	set("claude", "session", "123", "12345", cmd, "idle")
	if got, ok := guardedWrite(ctx, entry, "claude", "session", Projection{State: "working"}, false, nil); !ok || projection() != "working||" || got.Pane.ID != pane {
		t.Fatalf("the full guard refused: projection=%q", projection())
	}

	// Each predicate alone must refuse without touching the projection.
	for _, tc := range []struct {
		name                           string
		agent, id, pid, start, command string
	}{
		{"foreign agent", "codex", "session", "123", "12345", cmd},
		{"foreign session", "claude", "other", "123", "12345", cmd},
		{"other pid", "claude", "session", "999", "12345", cmd},
		{"other start", "claude", "session", "123", "99999", cmd},
		{"another program", "claude", "session", "123", "12345", "zsh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set(tc.agent, tc.id, tc.pid, tc.start, tc.command, "idle")
			if _, ok := guardedWrite(ctx, entry, "claude", "session", Projection{State: "working"}, false, nil); ok {
				t.Fatal("the guard accepted a false predicate")
			}
			if got := projection(); got != "idle|old|111" {
				t.Fatalf("projection changed on refusal: %q", got)
			}
		})
	}

	// A dead pane refuses too, even when every option still matches.
	set("claude", "session", "123", "12345", cmd, "idle")
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
	set("claude", "session", "123", "12345", "true", "idle")
	if _, ok := guardedWrite(ctx, entry, "claude", "session", Projection{State: "working"}, false, nil); ok {
		t.Fatal("the guard accepted a dead pane")
	}
	if got := projection(); got != "idle|old|111" {
		t.Fatalf("projection changed on a dead pane: %q", got)
	}
}

func TestValuesWithTmuxSyntaxAreNeverUsed(t *testing.T) {
	for _, s := range []session.Shown{
		{Server: "/tmp/tmux.sock", Pane: "%1", PID: 10, Start: "x}#{run-shell,touch /tmp/pwn}"},
		{Server: "/tmp/tmux.sock", Pane: "%x", PID: 10, Start: "10"},
		{Server: "/tmp/tmux.sock", Pane: "%1", PID: 0, Start: "10"},
	} {
		if _, ok := guardedWrite(context.Background(), s, "claude", "session", Projection{State: "working"}, false, nil); ok {
			t.Fatalf("entry %+v was used", s)
		}
	}
	if _, ok := guardedWrite(context.Background(), session.Shown{Server: "/tmp/tmux.sock", Pane: "%1", PID: 10, Start: "10"},
		"claude", "bad id #{}", Projection{State: "working"}, false, nil); ok {
		t.Fatal("a session id with tmux syntax was used")
	}
}

// Project writes the latest state to the panes the record lists, reads the
// options it is asked for with them, and unlists a pane that no longer
// shows the session.
func TestProjectWritesListedPanesAndUnlistsRefusedOnes(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	if _, err := srv.Run(ctx, "set-option", "-g", "@tmag-alert", "finished"); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetPaneOptions(ctx, pane, map[string]string{
		"agent": proc.Claude, "session": "s1", "pid": "123", "start": "12345", "evidence": Process, "cmd": cmd, "state": "working",
	}, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := Env{Log: eventlog.Open(dir), Store: session.Store{Dir: dir}}
	listed := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	gone := session.Shown{Server: srv.Socket, Pane: "%99", PID: 124, Start: "12346"}
	old := session.Record{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Shown: []session.Shown{listed, gone}}
	latest := old
	latest.Main, latest.Since = adapter.Idle, time.Unix(200, 0).UTC()
	if err := env.Store.Put(ctx, latest); err != nil {
		t.Fatal(err)
	}

	written, refused, err := env.Project(ctx, old, "@tmag-alert")
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || !written[0].Newer || !written[0].Read || written[0].Options["@tmag-alert"] != "finished" {
		t.Fatalf("written %+v", written)
	}
	if got := written[0].Pane.Options["state"]; got != adapter.Idle {
		t.Fatalf("an older hook wrote its own state, %q, over the latest", got)
	}
	if len(refused) != 1 || refused[0] != gone {
		t.Fatalf("refused %+v", refused)
	}
	stored, _, err := env.Store.Get(ctx, proc.Claude, "s1")
	if err != nil || !stored.Lists(listed) || stored.Lists(gone) {
		t.Fatalf("listed after Project: %+v, %v", stored.Shown, err)
	}
}
