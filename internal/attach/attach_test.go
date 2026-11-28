package attach

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

const (
	threadA = "01a1a0b8-ed6f-7b52-b68a-fe4c0891964d"
	threadB = "01a1a0be-5b8b-77b1-b0dc-3230fb6d76f6"
	subA    = "01a1a0bb-5bd7-7600-b1dc-46116a20a299"
	sock    = "/tmp/tmux-1/default"
)

// shown renders an id the way the Codex 0.166 thread-id item does.
func shown(id string) string { return id[:titleIDLen] + "..." }

func TestTitleIDs(t *testing.T) {
	for title, want := range map[string][]string{
		// Codex 0.166, terminal_title = [activity, thread-name, project-name, thread-id].
		"proj | " + shown(threadA):                          {threadA[:titleIDLen]},
		"⠼ 运行 echo hi | proj | " + shown(threadA):           {threadA[:titleIDLen]},
		"[ ! ] Action Required | name | " + shown(threadA):  {threadA[:titleIDLen]},
		"⠼ " + shown(threadA) + " | proj":                   {threadA[:titleIDLen]}, // activity beside the id
		"proj | " + shown(threadA) + " | " + shown(threadB): {threadA[:titleIDLen], threadB[:titleIDLen]},
		// Names come from prompts; one that mentions an id is still a name.
		"see " + shown(threadA) + " | proj": nil,
		threadA + " | proj":                 nil, // a full id is a thread-title or a name
		"GPT-6 | proj":                      nil,
		"":                                  nil,
	} {
		if got := TitleIDs(title); !reflect.DeepEqual(got, want) {
			t.Errorf("TitleIDs(%q) = %q, want %q", title, got, want)
		}
	}
}

type fixture struct {
	panes []tmux.Pane
	procs []proc.Process
}

// pane adds a pane whose shell runs an agent client.
func (f *fixture) pane(id string, shellPID int, agent string, clientPID int, title string, opts map[string]string) {
	if opts == nil {
		opts = map[string]string{}
	}
	f.panes = append(f.panes, tmux.Pane{Server: sock, ID: id, PID: shellPID, Command: agent, Title: title, Options: opts})
	f.procs = append(f.procs, proc.Process{PID: shellPID, PPID: 1, Comm: "zsh", Args: "-zsh"})
	if agent != "" {
		f.procs = append(f.procs, proc.Process{PID: clientPID, PPID: shellPID, Comm: agent, Args: agent, Start: "s" + id})
	}
}

func (f *fixture) world() *World { return &World{Panes: f.panes, Proc: proc.NewTable(f.procs)} }

func stored(a Attachment) map[string]string {
	return map[string]string{
		optAgent: a.Agent, optSession: a.Session, optPID: strconv.Itoa(a.PID), optStart: a.Start, optEvidence: a.Evidence,
	}
}

func codexSession(id string, subagents ...string) session.Record {
	return session.Record{Agent: proc.Codex, ID: id, Main: "idle", Subagents: subagents,
		Host: session.Host{PID: 900, Role: proc.Service}}
}

func linkOf(t *testing.T, links []Link, pane string) Link {
	t.Helper()
	for _, l := range links {
		if l.Client.Pane.ID == pane {
			return l
		}
	}
	t.Fatalf("no link for %s", pane)
	return Link{}
}

func TestNameLikeTitleItemsProveNothing(t *testing.T) {
	// The third review's P1: a custom terminal_title shows the model, and a
	// thread is named like it. Names are no evidence, whatever they match.
	var f fixture
	f.pane("%1", 100, proc.Codex, 101, "GPT-6 | proj", nil)
	f.procs = append(f.procs, proc.Process{PID: 900, PPID: 1, Comm: "codex", Args: "codex app-server --managed-daemon"})
	l := linkOf(t, Links(f.world(), []session.Record{codexSession(threadA)}), "%1")
	if l.Session != nil {
		t.Fatalf("a title without a thread id attached %s", l.Session.ID)
	}
}

func TestTitleIDLinks(t *testing.T) {
	var f fixture
	f.pane("%1", 100, proc.Codex, 101, "proj | "+shown(threadA), nil)
	f.pane("%2", 200, proc.Codex, 201, "proj | "+shown(subA), nil)
	f.pane("%3", 300, proc.Codex, 301, "proj | "+shown(threadB), nil) // not a session tmag knows
	f.pane("%4", 400, proc.Codex, 401, "proj", nil)                   // no thread-id item
	links := Links(f.world(), []session.Record{codexSession(threadA, subA)})

	l := linkOf(t, links, "%1")
	if l.Session == nil || l.Session.ID != threadA || l.Evidence != TitleID || l.View != ViewMain || l.Stored {
		t.Fatalf("%%1: %+v", l)
	}
	l = linkOf(t, links, "%2")
	if l.Session == nil || l.Session.ID != threadA || l.View != ViewSubagent {
		t.Fatalf("%%2 viewing a subagent: %+v", l)
	}
	for _, p := range []string{"%3", "%4"} {
		if l := linkOf(t, links, p); l.Session != nil {
			t.Fatalf("%s attached to %s", p, l.Session.ID)
		}
	}
}

func TestStoredAttachmentHoldsWhileItsClientRuns(t *testing.T) {
	rec := session.Record{Agent: proc.Claude, ID: "c1", Main: "idle"}
	a := Attachment{Agent: proc.Claude, Session: "c1", PID: 101, Start: "s%1", Evidence: Process}
	var f fixture
	f.pane("%1", 100, proc.Claude, 101, "", stored(a))
	l := linkOf(t, Links(f.world(), []session.Record{rec}), "%1")
	if l.Session == nil || !l.Stored || l.Evidence != Process {
		t.Fatalf("live attachment not kept: %+v", l)
	}

	// The same pid, started again: another process.
	f = fixture{}
	f.pane("%1", 100, proc.Claude, 101, "", stored(a))
	f.procs[1].Start = "later"
	if l := linkOf(t, Links(f.world(), []session.Record{rec}), "%1"); l.Session != nil {
		t.Fatalf("attachment survived its client: %+v", l)
	}

	// The session is gone.
	f = fixture{}
	f.pane("%1", 100, proc.Claude, 101, "", stored(a))
	if l := linkOf(t, Links(f.world(), nil), "%1"); l.Session != nil {
		t.Fatalf("attachment survived its session: %+v", l)
	}
}

func TestCodexTitleOverridesAProcessAttachment(t *testing.T) {
	// codex --no-daemon, which also shows thread-id, resumed another thread.
	rec := session.Record{Agent: proc.Codex, ID: threadA, Main: "idle"}
	other := session.Record{Agent: proc.Codex, ID: threadB, Main: "idle"}
	a := Attachment{Agent: proc.Codex, Session: threadA, PID: 101, Start: "s%1", Evidence: Process}
	var f fixture
	f.pane("%1", 100, proc.Codex, 101, "proj | "+shown(threadB), stored(a))
	l := linkOf(t, Links(f.world(), []session.Record{rec, other}), "%1")
	if l.Session == nil || l.Session.ID != threadB || l.Evidence != TitleID || l.Stored {
		t.Fatalf("pane kept showing the old thread: %+v", l)
	}
	// Without the thread-id item the process attachment stands.
	f = fixture{}
	f.pane("%1", 100, proc.Codex, 101, "proj", stored(a))
	if l := linkOf(t, Links(f.world(), []session.Record{rec, other}), "%1"); l.Session == nil || l.Session.ID != threadA {
		t.Fatalf("process attachment dropped: %+v", l)
	}
}

func TestClientOfRejectsNestedAndServiceHosts(t *testing.T) {
	var f fixture
	f.pane("%1", 100, proc.Claude, 101, "", nil)
	// codex exec run by Claude's Bash tool, inside the same pane.
	f.procs = append(f.procs,
		proc.Process{PID: 102, PPID: 101, Comm: "bash", Args: "bash -c codex exec"},
		proc.Process{PID: 103, PPID: 102, Comm: "codex", Args: "codex exec hi"},
		proc.Process{PID: 900, PPID: 1, Comm: "codex", Args: "codex app-server --managed-daemon"},
	)
	w := f.world()
	host := func(pid int) proc.Host {
		h, _ := w.Proc.HostOf(pid)
		return h
	}
	if c, ok := w.ClientOf(host(101)); !ok || c.Pane.ID != "%1" {
		t.Fatalf("pane's own agent not its client: %+v %v", c, ok)
	}
	if _, ok := w.ClientOf(host(103)); ok {
		t.Fatal("a nested agent claimed the pane")
	}
	if _, ok := w.ClientOf(host(900)); ok {
		t.Fatal("a daemon claimed a pane")
	}
}

// A declared agent is its pane's client by the process its hooks proved:
// before its pane shows it, while no other client is there; after, while
// that process runs there and its record names it as its host.
func TestDeclaredClients(t *testing.T) {
	rec := session.Record{Agent: "gemini", ID: "g1", Main: "idle", Host: session.Host{PID: 101, Start: "s%1", Role: proc.Terminal}}
	host := proc.Host{Process: proc.Process{PID: 101, PPID: 100, Comm: "gemini", Args: "gemini", Start: "s%1"}, Kind: "gemini", Role: proc.Terminal}
	var f fixture
	f.pane("%1", 100, "gemini", 101, "", nil)
	w := f.world()
	if _, ok := w.ClientIn(f.panes[0]); ok {
		t.Fatal("a pane shows a declared agent by its program's name")
	}
	c, ok := w.ClientOf(host)
	if !ok || c.Pane.ID != "%1" || c.Agent != "gemini" || c.Process.PID != 101 || !w.Runs(c) {
		t.Fatalf("a declared agent is not its pane's own: %+v %v", c, ok)
	}

	// Its pane shows it: the attachment names the client, and holds while
	// its record names that process.
	a := Attachment{Agent: "gemini", Session: "g1", PID: 101, Start: "s%1", Evidence: Process}
	f = fixture{}
	f.pane("%1", 100, "gemini", 101, "", stored(a))
	// Its tool runs Claude Code, which tmag would take for the pane's agent.
	f.procs = append(f.procs, proc.Process{PID: 102, PPID: 101, Comm: "claude", Args: "claude -p hi"})
	l := linkOf(t, Links(f.world(), []session.Record{rec}), "%1")
	if l.Session == nil || l.Session.ID != "g1" || !l.Stored || l.Client.Agent != "gemini" {
		t.Fatalf("a declared agent's attachment does not hold: %+v", l)
	}
	if _, ok := f.world().ClientOf(proc.Host{Process: proc.Process{PID: 102, Comm: "claude"}, Kind: proc.Claude, Role: proc.Terminal}); ok {
		t.Fatal("an agent a declared agent runs claimed its pane")
	}
	moved := rec
	moved.Host.PID = 555
	if l := linkOf(t, Links(f.world(), []session.Record{moved}), "%1"); l.Session != nil {
		t.Fatalf("an attachment held for a process its record does not name: %+v", l)
	}
	serving := rec
	serving.Host.Role = proc.Service
	if l := linkOf(t, Links(f.world(), []session.Record{serving}), "%1"); l.Session != nil {
		t.Fatalf("a pane shows a declared session that serves a program: %+v", l)
	}

	// Its process ended and the pane's shell runs Claude Code: the pane's
	// client is Claude's.
	f = fixture{}
	f.pane("%1", 100, proc.Claude, 103, "", stored(a))
	if c, ok := f.world().ClientIn(f.panes[0]); !ok || c.Agent != proc.Claude {
		t.Fatalf("a gone declared agent kept its pane: %+v %v", c, ok)
	}

	// Another client in the pane keeps a declared agent off it.
	f = fixture{}
	f.pane("%1", 100, proc.Claude, 101, "", nil)
	f.procs = append(f.procs, proc.Process{PID: 104, PPID: 101, Comm: "gemini", Args: "gemini", Start: "x"})
	nested := proc.Host{Process: proc.Process{PID: 104, Comm: "gemini", Start: "x"}, Kind: "gemini", Role: proc.Terminal}
	if c, ok := f.world().ClientOf(nested); ok {
		t.Fatalf("a declared agent claimed another agent's pane: %+v", c)
	}
}

func TestLiveDropsSessionsWhoseHostExited(t *testing.T) {
	w := &World{Proc: proc.NewTable([]proc.Process{{PID: 900, Start: "1"}})}
	running := session.Record{Agent: proc.Codex, ID: "a", Host: session.Host{PID: 900, Start: "1"}}
	restarted := session.Record{Agent: proc.Codex, ID: "b", Host: session.Host{PID: 900, Start: "0"}}
	unknown := session.Record{Agent: proc.Claude, ID: "c"}
	live, exited := Live(w, []session.Record{running, restarted, unknown})
	if len(live) != 2 || len(exited) != 1 || exited[0].ID != "b" {
		t.Fatalf("live %v, exited %v", live, exited)
	}
}

func TestFromPaneReadsOnlyAttachmentsTmagWrites(t *testing.T) {
	valid := Attachment{Agent: proc.Claude, Session: "c1", PID: 101, Start: "s%1", Evidence: Process}
	for name, spoil := range map[string]func(o map[string]string){
		"no agent's name":   func(o map[string]string) { o[optAgent] = "bash" },
		"path as session":   func(o map[string]string) { o[optSession] = "../c1" },
		"oversized session": func(o map[string]string) { o[optSession] = strings.Repeat("c", 200) },
		"no process":        func(o map[string]string) { o[optPID] = "0" },
		"unknown evidence":  func(o map[string]string) { o[optEvidence] = "name" },
	} {
		o := stored(valid)
		spoil(o)
		if a, ok := FromPane(tmux.Pane{Server: sock, ID: "%1", Options: o}); ok {
			t.Errorf("%s read as %+v", name, a)
		}
	}
	if a, ok := FromPane(tmux.Pane{Server: sock, ID: "%1", Options: stored(valid)}); !ok || a != valid {
		t.Fatalf("a valid attachment read as %+v, %v", a, ok)
	}
}
