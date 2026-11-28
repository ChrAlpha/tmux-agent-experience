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

// callers is a world with a Claude Code pane, a person's pane, an oh-my-pi
// pane and a Codex daemon outside them, each running a tmag command.
func callers() (View, *attach.World) {
	w := &attach.World{
		Panes: []tmux.Pane{
			{Server: sock, ID: "%1", PID: 10, Command: "claude"},
			{Server: sock, ID: "%2", PID: 30, Command: "zsh"},
			{Server: sock, ID: "%3", PID: 40, Command: "omp"},
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
			// oh-my-pi in %3 runs its tool's command in process.
			{PID: 40, PPID: 1, Comm: "zsh"},
			{PID: 41, PPID: 40, Comm: "omp", Args: "omp", Start: "o"},
			{PID: 42, PPID: 41, Comm: "tmag"},
		}),
	}
	recs := []session.Record{
		{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Host: session.Host{PID: 11, Start: "c", Role: proc.Terminal}},
		{Agent: proc.Codex, ID: "th1", Main: adapter.Working, Subagents: []string{"sub1"}, Host: session.Host{PID: 20, Start: "d", Role: proc.Service}},
		{Agent: proc.Codex, ID: "th2", Main: adapter.Idle, Host: session.Host{PID: 20, Start: "d", Role: proc.Service}},
		{Agent: proc.OMP, ID: "o1", Main: adapter.Working, Host: session.Host{PID: 41, Start: "o", Role: proc.Terminal}},
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
		{"oh-my-pi naming its session", 42, map[string]string{"PI_SESSION_ID": "o1"}, Caller{Pane: pane("%3"), By: &eventlog.Actor{Agent: proc.OMP, Session: "o1"}}},
		{"oh-my-pi naming another", 42, map[string]string{"PI_SESSION_ID": "s1"}, Caller{Pane: pane("%3"), By: &eventlog.Actor{Agent: proc.OMP}}},
	} {
		if got := identify(v, w, c.pid, env(c.vars), nil); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v (by %+v), want %+v (by %+v)", c.name, got, got.By, c.want, c.want.By)
		}
	}
}

// A declared agent's tool is its agent's by the host its hooks proved; a
// program the person counts as an agent makes its tool an agent's whose
// session tmag cannot tell, even below an agent whose session it can;
// anything else is still the person.
func TestDeclaredAndCountedCallers(t *testing.T) {
	pane := func(id string) *eventlog.PaneRef { return &eventlog.PaneRef{Server: sock, ID: id} }
	w := &attach.World{
		Panes: []tmux.Pane{
			{Server: sock, ID: "%1", PID: 10, Command: "node"},
			{Server: sock, ID: "%2", PID: 30, Command: "aider"},
			{Server: sock, ID: "%3", PID: 50, Command: "gemini"},
			{Server: sock, ID: "%4", PID: 60, Command: "claude"},
		},
		Proc: proc.NewTable([]proc.Process{
			{PID: 1, Comm: "init"},
			// A declared agent in %1, its hooks' host, its tool running tmag.
			{PID: 10, PPID: 1, Comm: "zsh"},
			{PID: 11, PPID: 10, Comm: "node-MainThread", Argv: []string{"node", "/usr/local/bin/gemini"}, Start: "g"},
			{PID: 12, PPID: 11, Comm: "bash"},
			{PID: 13, PPID: 12, Comm: "tmag"},
			// An agent that adapted nothing in %2, run by Python.
			{PID: 30, PPID: 1, Comm: "zsh"},
			{PID: 31, PPID: 30, Comm: "python3", Argv: []string{"python3", "/home/me/.local/bin/aider"}},
			{PID: 32, PPID: 31, Comm: "tmag"},
			// Another gemini in %3 whose hooks never reported: its program's
			// name proves nothing.
			{PID: 50, PPID: 1, Comm: "zsh"},
			{PID: 51, PPID: 50, Comm: "gemini", Args: "gemini", Start: "x"},
			{PID: 52, PPID: 51, Comm: "tmag"},
			// Claude Code in %4 runs aider as its tool, whose tool runs tmag.
			{PID: 60, PPID: 1, Comm: "zsh"},
			{PID: 61, PPID: 60, Comm: "claude", Args: "claude", Start: "c"},
			{PID: 62, PPID: 61, Comm: "aider", Args: "aider"},
			{PID: 63, PPID: 62, Comm: "tmag"},
		}),
	}
	recs := []session.Record{
		{Agent: "gemini", ID: "g1", Main: adapter.Idle, Host: session.Host{PID: 11, Start: "g", Role: proc.Terminal}},
		{Agent: proc.Claude, ID: "c1", Main: adapter.Working, Host: session.Host{PID: 61, Start: "c", Role: proc.Terminal}},
	}
	v := Build(w, recs)
	none := func(string) string { return "" }
	if got := identify(v, w, 13, none, nil); got.By == nil || *got.By != (eventlog.Actor{Agent: "gemini", Session: "g1"}) || *got.Pane != *pane("%1") {
		t.Errorf("a declared agent's tool: %+v (by %+v)", got, got.By)
	}
	for _, pid := range []int{32, 52} {
		if got := identify(v, w, pid, none, nil); got.By != nil {
			t.Errorf("%d passes for an agent: %+v", pid, got.By)
		}
	}
	if got := identify(v, w, 63, none, nil); got.By == nil || *got.By != (eventlog.Actor{Agent: proc.Claude, Session: "c1"}) {
		t.Errorf("a tool below a program nobody counts is its agent's: %+v (by %+v)", got, got.By)
	}
	counted := parseAgents("aider, gemini bash", "Aider,aider")
	if !reflect.DeepEqual(counted, []string{"aider", "gemini"}) {
		t.Fatalf("@tmag-agents counts %v", counted)
	}
	for pid, want := range map[int]eventlog.Actor{32: {Agent: "aider"}, 52: {Agent: "gemini"}, 63: {Agent: "aider"}} {
		if got := identify(v, w, pid, none, counted); got.By == nil || *got.By != want {
			t.Errorf("%d: %+v (by %+v), want %+v", pid, got, got.By, want)
		}
	}
	if got := identify(v, w, 13, none, counted); got.By == nil || *got.By != (eventlog.Actor{Agent: "gemini", Session: "g1"}) {
		t.Errorf("a declared agent's host its program is counted: %+v (by %+v)", got, got.By)
	}
}

func TestCallerGates(t *testing.T) {
	v, w := callers()
	self := identify(v, w, 13, func(string) string { return "" }, nil)
	s1, _ := v.Session(proc.Claude, "s1")
	th1, _ := v.Session(proc.Codex, "th1")
	if err := self.selfGate(s1, "interrupt"); !isRefused(err, RefuseSelf) {
		t.Fatalf("an agent interrupting itself: %v", err)
	}
	if err := self.selfGate(th1, "interrupt"); err != nil {
		t.Fatalf("an agent interrupting another: %v", err)
	}
	// An agent whose session is untold is refused nothing as itself.
	untold := identify(v, w, 23, func(string) string { return "" }, nil)
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
	w.Panes = nil // no server to read @tmag-agents from
	ctx := context.Background()
	if c, err := callerFor(ctx, v, w); err != nil || c.By == nil {
		t.Fatal("ordinary CLI lost agent ancestry")
	}
	if c, err := callerFor(WithOwner(ctx, auth.Grant{}), v, w); err != nil || c.By == nil {
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
	owner, err := callerFor(WithOwner(ctx, grant), v, w)
	if err != nil || owner.By != nil || owner.Credential != issued.ID {
		t.Fatal("authenticated HTTP inherited launcher's agent identity")
	}
	var event eventlog.Event
	owner.stamp(&event)
	if event.By != nil || event.Credential != issued.ID {
		t.Fatal("owner provenance was not logged separately")
	}
}
