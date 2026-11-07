package query

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

const sock = "/tmp/tmux-1/default"

// callers is a world with a Claude Code pane, a person's pane and a Codex
// daemon outside both, each running a tmag command.
func callers() (View, *attach.World) {
	w := &attach.World{
		Panes: []tmux.Pane{
			{Server: sock, ID: "%1", PID: 10, Command: "claude"},
			{Server: sock, ID: "%2", PID: 30, Command: "zsh"},
		},
		Proc: proc.NewTable([]proc.Process{
			{PID: 1, Comm: "init"},
			// Claude Code in %1, its Bash tool running tmag.
			{PID: 10, PPID: 1, Comm: "zsh"},
			{PID: 11, PPID: 10, Comm: "claude", Args: "claude", Start: "c"},
			{PID: 12, PPID: 11, Comm: "zsh"},
			{PID: 13, PPID: 12, Comm: "tmag"},
			// The Codex daemon; a sandbox helper runs its tool's command.
			{PID: 20, PPID: 1, Comm: "codex", Args: "codex app-server", Argv: []string{"codex", "app-server"}, Start: "d"},
			{PID: 21, PPID: 20, Comm: "codex-linux-sandbox", Args: "codex-linux-sandbox"},
			{PID: 22, PPID: 21, Comm: "bash"},
			{PID: 23, PPID: 22, Comm: "tmag"},
			// A person at a shell in %2.
			{PID: 30, PPID: 1, Comm: "zsh"},
			{PID: 31, PPID: 30, Comm: "tmag"},
		}),
	}
	recs := []session.Record{
		{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Host: session.Host{PID: 11, Start: "c", Role: proc.Terminal}},
		{Agent: proc.Codex, ID: "th1", Main: adapter.Working, Subagents: []string{"sub1"}, Host: session.Host{PID: 20, Start: "d", Role: proc.Service}},
		{Agent: proc.Codex, ID: "th2", Main: adapter.Idle, Host: session.Host{PID: 20, Start: "d", Role: proc.Service}},
	}
	return Build(w, recs), w
}

func TestCallerIsProvenByAncestry(t *testing.T) {
	v, w := callers()
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	pane := func(id string) *eventlog.PaneRef { return &eventlog.PaneRef{Server: sock, ID: id} }
	for _, c := range []struct {
		name string
		pid  int
		vars map[string]string
		want Caller
	}{
		{"a person", 31, nil, Caller{Pane: pane("%2")}},
		// Claude's tool: its agent hosts one session.
		{"claude", 13, nil, Caller{Pane: pane("%1"), By: &eventlog.Actor{Agent: proc.Claude, Session: "s1"}}},
		{"claude naming its session", 13, map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}, Caller{Pane: pane("%1"), By: &eventlog.Actor{Agent: proc.Claude, Session: "s1"}}},
		// A variable naming a session its agent does not host proves nothing.
		{"claude naming another", 13, map[string]string{"CLAUDE_CODE_SESSION_ID": "th2"}, Caller{Pane: pane("%1"), By: &eventlog.Actor{Agent: proc.Claude}}},
		// The daemon hosts two threads: its variable tells which, past the
		// sandbox helper; a subagent's thread is its session's.
		{"codex thread", 23, map[string]string{"CODEX_THREAD_ID": "th2"}, Caller{By: &eventlog.Actor{Agent: proc.Codex, Session: "th2"}}},
		{"codex subagent", 23, map[string]string{"CODEX_THREAD_ID": "sub1"}, Caller{By: &eventlog.Actor{Agent: proc.Codex, Session: "th1", Subagent: "sub1"}}},
		{"codex untold", 23, nil, Caller{By: &eventlog.Actor{Agent: proc.Codex}}},
		// Claude's variable is no Codex thread.
		{"codex with claude's variable", 23, map[string]string{"CLAUDE_CODE_SESSION_ID": "th1"}, Caller{By: &eventlog.Actor{Agent: proc.Codex}}},
	} {
		if got := identify(v, w, c.pid, env(c.vars)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v (by %+v), want %+v (by %+v)", c.name, got, got.By, c.want, c.want.By)
		}
	}
}

func TestCallerGates(t *testing.T) {
	v, w := callers()
	self := identify(v, w, 13, func(string) string { return "" })
	s1, _ := v.Session(proc.Claude, "s1")
	th1, _ := v.Session(proc.Codex, "th1")
	if err := self.selfGate(s1, "interrupt"); !isRefused(err, RefuseSelf) {
		t.Fatalf("an agent interrupting itself: %v", err)
	}
	if err := self.selfGate(th1, "interrupt"); err != nil {
		t.Fatalf("an agent interrupting another: %v", err)
	}
	// An agent whose session is untold is refused nothing as itself.
	untold := identify(v, w, 23, func(string) string { return "" })
	if err := untold.selfGate(th1, "prompt"); err != nil {
		t.Fatalf("untold: %v", err)
	}
	var ev eventlog.Event
	self.stamp(&ev)
	if ev.From == nil || ev.From.ID != "%1" || ev.By == nil || ev.By.Session != "s1" {
		t.Fatalf("stamped %+v", ev)
	}
}

func TestOnlyAValidatedGrantEstablishesOwnerAuthority(t *testing.T) {
	v, w := callers()
	w.Proc = proc.NewTable([]proc.Process{{PID: 1}, {PID: 11, PPID: 1, Comm: "claude", Args: "claude", Start: "c"}, {PID: os.Getpid(), PPID: 11, Comm: "tmag"}})
	ctx := context.Background()
	if c := callerFor(ctx, v, w); c.By == nil {
		t.Fatal("ordinary CLI lost agent ancestry")
	}
	if c := callerFor(WithOwner(ctx, auth.Grant{}), v, w); c.By == nil {
		t.Fatal("zero grant claimed owner")
	}
	store := auth.Store{Dir: t.TempDir()}
	issued, err := store.Issue(ctx, "owner", 0)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	owner := callerFor(WithOwner(ctx, grant), v, w)
	if owner.By != nil || owner.Credential != issued.ID {
		t.Fatal("authenticated HTTP inherited launcher's agent identity")
	}
	var event eventlog.Event
	owner.stamp(&event)
	if event.By != nil || event.Credential != issued.ID {
		t.Fatal("owner provenance was not logged separately")
	}
}
