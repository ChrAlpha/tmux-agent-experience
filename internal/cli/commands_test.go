package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// A verb names the pane it went through as list prints it and targets take
// it: a bare %N is a pane of the caller's own server, so a pane of another
// is named server:%N.
func TestDeliveryNamesThePaneWithItsServer(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	for _, c := range []struct{ server, want string }{
		{tmux.DefaultServer().Socket, "sent to 1787a44e (%3) seq 7\n"},
		{filepath.Join(tmux.SocketDir(), "other"), "sent to 1787a44e (other:%3) seq 7\n"},
		{"/run/agents/tmux.sock", "sent to 1787a44e (/run/agents/tmux.sock:%3) seq 7\n"},
	} {
		var stdout bytes.Buffer
		a := app{ctx: context.Background(), log: eventlog.Open(t.TempDir()), stdout: &stdout}
		a.reportDelivery("sent to", query.Delivery{
			Session: query.Session{ID: "1787a44e-5b0c-4c39-9a6e-0d6c3c2f7b10"},
			Pane:    &query.Pane{Server: c.server, ID: "%3"},
			Via:     "pane", Seq: 7,
		})
		if got := stdout.String(); got != c.want {
			t.Errorf("a delivery through a pane of %s printed %q, want %q", c.server, got, c.want)
		}
	}
}

// A receipt names its session as a target resolves it, among the sessions
// that run: by more than 8 characters where another's id begins the same,
// also when the session it names has ended and has no record left, and by
// no more for a record whose host exited without SessionEnd.
func TestReceiptsNameSessionsAsTargetsResolveThem(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	log := eventlog.Open(t.TempDir())
	hub, worker, gone := "01a218ba-bdcd-7e83-861b-602aeb7d5f1e", "01a218ba-c97f-7830-8126-10d0a894cf82", "01a218ba-c98f-7830-8126-10d0a894cf82"
	for _, rec := range []session.Record{
		{Agent: "codex", ID: hub},
		{Agent: "codex", ID: gone, Host: session.Host{PID: 1 << 30}}, // no system has this pid
	} {
		if err := (session.Store{Dir: log.Dir}).Put(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	var stdout bytes.Buffer
	a := app{ctx: context.Background(), log: log, stdout: &stdout}
	a.reportDelivery("sent to", query.Delivery{Session: query.Session{ID: worker}, Via: "service", Seq: 7})
	a.reportWait(query.Session{ID: worker}, adapter.Idle, "", "", true)
	a.reportWait(query.Session{ID: hub}, adapter.Idle, "", "", true)
	if got, want := stdout.String(), "sent to 01a218ba-c (service) seq 7\n01a218ba-c idle\n01a218ba idle\n"; got != want {
		t.Errorf("receipts are %q, want %q", got, want)
	}
}

// A message Claude Code will hold is still delivered, and says so after the
// delivery line; it is not a refusal.
func TestDeliverySaysWhenClaudeHoldsAMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := app{ctx: context.Background(), log: eventlog.Open(t.TempDir()), stdout: &stdout, stderr: &stderr}
	a.reportDelivery("sent to", query.Delivery{
		Session: query.Session{ID: "1787a44e-5b0c-4c39-9a6e-0d6c3c2f7b10"},
		Via:     "inbox", Seq: 7, Held: true,
	})
	if got, want := stdout.String(), "sent to 1787a44e (inbox) seq 7\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), heldLine+"\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	for _, held := range []bool{true, false} {
		var doc bytes.Buffer
		a := app{ctx: context.Background(), stdout: &doc, stderr: &bytes.Buffer{}, json: true}
		a.reportDelivery("sent to", query.Delivery{Session: query.Session{ID: "s1"}, Via: "inbox", Seq: 7, Held: held})
		var got map[string]any
		if err := json.Unmarshal(doc.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if v, ok := got["held"]; ok != held || (held && v != true) {
			t.Errorf("--json with held %v: %s", held, doc.String())
		}
	}
}

// A blocked session's open requests are listed by inspect without --json,
// under the ids request and answer take: the skill sends the person there
// for a blocked peer.
func TestInspectListsOpenRequests(t *testing.T) {
	// Inspect loads the world: keep its tmux discovery off the developer's
	// server.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	log := eventlog.Open(t.TempDir())
	bash := adapter.Prompt{Tool: "Bash"}
	write := adapter.Prompt{Agent: "a7f3e1c29b04", Tool: "Write"}
	elicit := adapter.Prompt{Tool: "@elicitation"}
	rec := session.Record{
		Agent: "claude", ID: "1787a44e-5b0c-4c39-9a6e-0d6c3c2f7b10", Main: adapter.Working,
		Requests: []session.Request{{ID: "3fdce4c70dfa", Prompt: bash}, {ID: "9b41e07a2c55", Prompt: write}, {ID: "c0d2a8f61e93", Prompt: elicit}},
	}
	if err := (session.Store{Dir: log.Dir}).Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := app{ctx: context.Background(), log: log, stdout: &stdout, stderr: &stderr}
	if code := a.run([]string{"inspect", "1787"}); code != ExitOK {
		t.Fatalf("inspect: %d %s", code, stderr.String())
	}
	want := "request: 3fdce4c70dfa (permission, Bash)\n" +
		"request: 9b41e07a2c55 (permission, Write, subagent a7f3e1c29b04)\n" +
		"request: c0d2a8f61e93 (question)\n"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("inspect printed\n%s\nwithout\n%s", stdout.String(), want)
	}
}
