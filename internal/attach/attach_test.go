package attach

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
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

func TestLiveDropsSessionsWhoseHostExited(t *testing.T) {
	w := &World{Proc: proc.NewTable([]proc.Process{{PID: 900, Start: "1"}})}
	running := session.Record{Agent: proc.Codex, ID: "a", Host: session.Host{PID: 900, Start: "1"}}
	restarted := session.Record{Agent: proc.Codex, ID: "b", Host: session.Host{PID: 900, Start: "0"}}
	unknown := session.Record{Agent: proc.Claude, ID: "c"}
	live, exited := Live(w, []session.Record{running, restarted, unknown})
	if len(live) != 3 || len(exited) != 0 {
		t.Fatalf("live %v, exited %v", live, exited)
	}
}

func TestReconcileFinishesWhatAWriterLeft(t *testing.T) {
	dir := t.TempDir()
	env := Env{Log: eventlog.Open(dir), Store: session.Store{Dir: dir}}
	ctx := context.Background()
	// Crashed between storing and logging: a change, an end, and a session
	// whose process exited without either.
	unlogged := session.Record{Agent: proc.Codex, ID: "a", Main: "idle", Change: "Stop"}
	ended := session.Record{Agent: proc.Codex, ID: "b", Main: "idle", Logged: session.Report{State: "idle"}, Ended: eventlog.End}
	exited := session.Record{Agent: proc.Claude, ID: "c", Main: "idle", Logged: session.Report{State: "idle"}, Host: session.Host{PID: 77, Start: "1"}}
	for _, r := range []session.Record{unlogged, ended, exited} {
		if err := env.Store.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	w := &World{Proc: proc.NewTable(nil)}
	env.Snapshot = func(context.Context) (*proc.Table, error) { return proc.NewTable(nil), nil }
	if live, gone := Live(w, []session.Record{unlogged, ended, exited}); len(live) != 2 || len(gone) != 1 {
		t.Fatalf("live %v, gone %v", live, gone)
	}
	Reconcile(ctx, env, w)
	Reconcile(ctx, env, w) // a second reconciliation finds nothing left to do

	all, _, err := env.Log.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range all {
		got = append(got, e.Session+":"+e.Event+":"+e.State)
	}
	slices.Sort(got)
	if want := []string{"a:Stop:idle", "b:SessionEnd:", "c:Exit:"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	recs, _ := env.Store.All()
	if len(recs) != 1 || recs[0].ID != "a" || recs[0].Unlogged() {
		t.Fatalf("sessions left: %+v", recs)
	}
}

func TestAttachmentBeingDetachedNoLongerHolds(t *testing.T) {
	rec := session.Record{Agent: proc.Claude, ID: "c1", Main: "idle"}
	a := Attachment{Agent: proc.Claude, Session: "c1", PID: 101, Start: "s%1", Evidence: Process}
	for change, want := range map[string]bool{"": true, eventlog.Attach: true, eventlog.Detach: false} {
		opts := stored(a)
		if change != "" {
			opts[optChange] = change
		}
		var f fixture
		f.pane("%1", 100, proc.Claude, 101, "", opts)
		w := f.world()
		got, ok := FromPane(f.panes[0])
		if !ok || got.Change != change || !got.same(a) {
			t.Fatalf("change %q read as %+v, %v", change, got, ok)
		}
		if l := linkOf(t, Links(w, []session.Record{rec}), "%1"); (l.Session != nil) != want {
			t.Errorf("change %q: link %+v", change, l)
		}
		if shown := w.Attached(rec); (len(shown) == 1) != want {
			t.Errorf("change %q: attached %+v", change, shown)
		}
	}
}
