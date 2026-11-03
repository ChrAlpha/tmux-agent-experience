package bind

import (
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexidx"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

const (
	sid   = "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b"
	other = "019a2b3c-9999-7f60-8a9b-0c1d2e3f4a5b"
)

// world builds panes %1..%n whose shells have pid 100*i, each running the
// given agent (pid 100*i+1) unless agent is "".
type paneSpec struct {
	agent, title, path string
	opts               map[string]string
}

func world(specs ...paneSpec) *World {
	ps := []proc.Process{{PID: 1, PPID: 0, Comm: "init"}}
	var panes []tmux.Pane
	for i, s := range specs {
		shell := 100 * (i + 1)
		ps = append(ps, proc.Process{PID: shell, PPID: 1, Comm: "zsh"})
		if s.agent != "" {
			ps = append(ps, proc.Process{PID: shell + 1, PPID: shell, Comm: s.agent, Args: s.agent})
		}
		opts := s.opts
		if opts == nil {
			opts = map[string]string{}
		}
		panes = append(panes, tmux.Pane{
			Server: "/tmp/tmux-1/default", ID: "%" + string(rune('1'+i)), PID: shell,
			Command: s.agent, Path: s.path, Title: s.title, Options: opts,
		})
	}
	// A Codex daemon with a hook child, outside any pane.
	ps = append(ps,
		proc.Process{PID: 900, PPID: 1, Comm: "codex", Args: "codex app-server --managed-daemon"},
		proc.Process{PID: 901, PPID: 900, Comm: "sh", Args: "sh -c tmag hook codex"},
		proc.Process{PID: 902, PPID: 901, Comm: "tmag", Args: "tmag hook codex"},
	)
	return &World{Panes: panes, Proc: proc.NewTable(ps)}
}

// resolveWith resolves r with the given Codex thread index.
func resolveWith(w *World, t Threads, r Request) Result {
	w.Threads = t
	return Resolve(w, r)
}

// threads is an in-memory thread index.
type threads map[string]string

func (t threads) Name(id string) string { return t[id] }

func (t threads) Showing(shown string) []string {
	var ids []string
	for id, n := range t {
		if codexidx.Display(n) == shown {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestProcessEvidence(t *testing.T) {
	w := world(paneSpec{agent: "claude"}, paneSpec{agent: "claude"})
	// hook 203 <- sh 202 <- claude 201 <- zsh 200 (pane %2)
	w.Proc = proc.NewTable(append(w.Proc.All(),
		proc.Process{PID: 202, PPID: 201, Comm: "sh"},
		proc.Process{PID: 203, PPID: 202, Comm: "tmag"},
	))
	r := Resolve(w, Request{Agent: proc.Claude, SessionID: sid, PID: 203})
	if r.Evidence != Process || r.Pane.ID != "%2" {
		t.Fatalf("got %+v", r)
	}
}

func TestNestedAgentDoesNotBindByProcess(t *testing.T) {
	for _, nested := range []string{"codex", "claude"} {
		w := world(paneSpec{agent: "claude"})
		// claude 101 <- bash tool 102 <- nested agent 103 <- hook 104
		w.Proc = proc.NewTable(append(w.Proc.All(),
			proc.Process{PID: 102, PPID: 101, Comm: "bash"},
			proc.Process{PID: 103, PPID: 102, Comm: nested, Args: nested + " exec"},
			proc.Process{PID: 104, PPID: 103, Comm: "tmag"},
		))
		if r := resolveWith(w, threads{}, Request{Agent: nested, SessionID: sid, PID: 104}); r.Evidence != Unbound {
			t.Fatalf("nested %s bound to %+v", nested, r)
		}
	}
}

func TestHookOutsideAnyAgentDoesNotBind(t *testing.T) {
	// A shell in the pane runs `tmag hook` directly: no agent vouches for it.
	w := world(paneSpec{agent: ""})
	w.Proc = proc.NewTable(append(w.Proc.All(), proc.Process{PID: 104, PPID: 100, Comm: "tmag"}))
	if r := Resolve(w, Request{Agent: proc.Claude, SessionID: sid, PID: 104}); r.Evidence != Unbound {
		t.Fatalf("bound %+v", r)
	}
}

func TestDaemonHookNeverBindsByProcess(t *testing.T) {
	// Even if the daemon were a child of a pane, the walk stops at it.
	w := world(paneSpec{agent: "codex"})
	procs := w.Proc.All()
	for i := range procs {
		if procs[i].PID == 900 {
			procs[i].PPID = 101
		}
	}
	w.Proc = proc.NewTable(procs)
	r := resolveWith(w, threads{}, Request{Agent: proc.Codex, SessionID: sid, PID: 902})
	if r.Evidence != Unbound {
		t.Fatalf("daemon hook bound to %+v", r)
	}
	// The pane's shell started the daemon itself, with root options first.
	w = world(paneSpec{})
	procs = w.Proc.All()
	for i := range procs {
		if procs[i].PID == 900 {
			procs[i].PPID = 100
			procs[i].Args = "codex -c model=example app-server --managed-daemon"
		}
	}
	w.Proc = proc.NewTable(procs)
	if r := resolveWith(w, threads{}, Request{Agent: proc.Codex, SessionID: sid, PID: 902}); r.Evidence != Unbound {
		t.Fatalf("hook of a daemon started with options bound to %+v", r)
	}
}

func TestTitleIDEvidence(t *testing.T) {
	w := world(
		paneSpec{agent: "codex", title: "codex"},
		paneSpec{agent: "codex", title: sid[:29] + "... Working", path: "/elsewhere"},
	)
	r := resolveWith(w, threads{}, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902})
	if r.Evidence != TitleID || r.Pane.ID != "%2" {
		t.Fatalf("got %+v", r)
	}
}

func TestTitleNameEvidence(t *testing.T) {
	idx := map[string]string{sid: "Refactor parser module", other: "Write release notes"}
	w := world(
		paneSpec{agent: "codex", title: "⠋ Write release notes | repo", path: "/repo"},
		paneSpec{agent: "codex", title: "⠋ Refactor parser module | repo", path: "/repo"},
		paneSpec{agent: "claude", title: "Refactor parser module", path: "/repo"},
	)
	r := resolveWith(w, threads(idx), Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902})
	if r.Evidence != TitleName || r.Pane.ID != "%2" {
		t.Fatalf("got %+v", r)
	}
	// Wrong cwd: no binding.
	r = resolveWith(w, threads(idx), Request{Agent: proc.Codex, SessionID: sid, Cwd: "/other", PID: 902})
	if r.Evidence != Unbound {
		t.Fatalf("cwd mismatch bound %+v", r)
	}
}

func TestTitleNameRefusesAmbiguity(t *testing.T) {
	name := "Refactor parser module"
	title := name + " | repo"
	// Two panes show the same name.
	w := world(paneSpec{agent: "codex", title: title, path: "/repo"}, paneSpec{agent: "codex", title: title, path: "/repo"})
	r := resolveWith(w, threads{sid: name}, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902})
	if r.Evidence != Unbound {
		t.Fatalf("two matching panes bound %+v", r)
	}
	// Two threads share the name.
	w = world(paneSpec{agent: "codex", title: title, path: "/repo"})
	r = resolveWith(w, threads{sid: name, other: name}, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902})
	if r.Evidence != Unbound {
		t.Fatalf("duplicate thread name bound %+v", r)
	}
}

func TestSessionEvidenceRequiresLiveAgent(t *testing.T) {
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101"}
	w := world(paneSpec{agent: "codex", opts: opts})
	r := resolveWith(w, threads{}, Request{Agent: proc.Codex, SessionID: sid, PID: 902})
	if r.Evidence != Session || r.Pane.ID != "%1" {
		t.Fatalf("got %+v", r)
	}
	// The agent exited: the stale record proves nothing.
	w = world(paneSpec{agent: "", opts: opts})
	r = resolveWith(w, threads{}, Request{Agent: proc.Codex, SessionID: sid, PID: 902})
	if r.Evidence != Unbound {
		t.Fatalf("stale record bound %+v", r)
	}
}

func TestRecordOfReplacedAgentIsStale(t *testing.T) {
	// The record names agent pid 99, but pid 101 runs in the pane now: the
	// agent was killed without SessionEnd and a new one started.
	w := world(paneSpec{agent: "claude", opts: map[string]string{"agent": "claude", "session": sid, "pid": "99", "state": "idle"}})
	if _, ok := w.Record(w.Panes[0]); ok {
		t.Fatal("record of a replaced agent accepted")
	}
	w = world(paneSpec{agent: "claude", opts: map[string]string{"agent": "claude", "session": sid, "pid": "101", "state": "idle"}})
	if rec, ok := w.Record(w.Panes[0]); !ok || rec.State != "idle" {
		t.Fatalf("live record rejected: %+v", rec)
	}
}

func TestTitleNameMatchesWholeItems(t *testing.T) {
	idx := threads{sid: "test"}
	w := world(paneSpec{agent: "codex", title: "fix tests | repo", path: "/repo"})
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != Unbound {
		t.Fatalf("substring bound %+v", r)
	}
	idx = threads{sid: "Ship release"}
	w = world(paneSpec{agent: "codex", title: "⠙ Ship release | repo", path: "/repo"})
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != TitleName {
		t.Fatalf("item not matched: %+v", r)
	}
}

func TestSessionEvidenceYieldsToThreadSwitch(t *testing.T) {
	idx := threads{sid: "Old thread", other: "New thread"}
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101"}
	// The TUI now shows another thread: the record no longer proves sid.
	w := world(paneSpec{agent: "codex", title: "New thread | repo", path: "/repo", opts: opts})
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != Unbound {
		t.Fatalf("stale session bound %+v", r)
	}
	// The other thread binds by its name.
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: other, Cwd: "/repo", PID: 902}); r.Evidence != TitleName {
		t.Fatalf("new thread not bound: %+v", r)
	}
	// A title without thread information keeps the binding.
	w = world(paneSpec{agent: "codex", title: "⠋ repo", path: "/repo", opts: opts})
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != Session {
		t.Fatalf("binding lost on a neutral title: %+v", r)
	}
}

func TestReadTitle(t *testing.T) {
	for title, want := range map[string]string{
		"⠋ Return PONG | github":                         "Return PONG/github",
		"Return PONG | github":                           "Return PONG/github",
		"github":                                         "/github",
		"⠦ github":                                       "/github",
		"● ⠦ Fix → parser | github":                      "Fix → parser/github",
		"[ ! ] Action Required | Fix tests | repo":       "Fix tests/repo",
		"● [ . ] Action Required | repo":                 "/repo",
		"019a2b3c-4d5e-7f60-8a9b-0c1... ⠋ Fix | x":       "019a2b3c-4d5e-7f60-8a9b-0c1...|Fix/x",
		"019a2b3c-4d5e-7f60-8a9b-0c1... | Fix tests | x": "019a2b3c-4d5e-7f60-8a9b-0c1...|Fix tests/x",
		"": "/",
	} {
		r := readTitle(title)
		if got := strings.Join(r.items, "|") + "/" + r.project; got != want {
			t.Errorf("readTitle(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestRecordYieldsToThreadSwitch(t *testing.T) {
	// The same Codex TUI moved from sid to other before any hook of other
	// arrived: every reader, not only the hook resolver, must drop sid.
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101", "state": "idle"}
	for _, title := range []string{other[:29] + "... | repo", "⠋ New thread | repo"} {
		w := world(paneSpec{agent: "codex", title: title, path: "/repo", opts: opts})
		w.Threads = threads{sid: "Old thread", other: "New thread"}
		if _, ok := w.Record(w.Panes[0]); ok {
			t.Errorf("title %q: record of the previous thread accepted", title)
		}
	}
	// A title naming no thread keeps the record.
	w := world(paneSpec{agent: "codex", title: "⠋ repo", path: "/repo", opts: opts})
	w.Threads = threads{sid: "Old thread", other: "New thread"}
	if _, ok := w.Record(w.Panes[0]); !ok {
		t.Error("record lost on a title without thread information")
	}
}

func TestTitleNameNeverOverridesAnotherThreadID(t *testing.T) {
	// The title shows other's id; sid's unique name happens to equal the
	// project name, and in a second case appears in the title as well.
	for name, title := range map[string]string{
		"repo":       other[:29] + "... | repo",
		"Fix parser": other[:29] + "... | Fix parser | repo",
	} {
		w := world(paneSpec{agent: "codex", title: title, path: "/repo"})
		if r := resolveWith(w, threads{sid: name}, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != Unbound {
			t.Errorf("name %q bound against the title's thread id: %+v", name, r)
		}
	}
}

func TestProjectNameIsNotAThreadName(t *testing.T) {
	w := world(paneSpec{agent: "codex", title: "⠋ repo", path: "/repo"})
	if r := resolveWith(w, threads{sid: "repo"}, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != Unbound {
		t.Fatalf("project item bound as a thread name: %+v", r)
	}
}

func TestAmbiguityIsNotResolvedByWeakerEvidence(t *testing.T) {
	// Two TUIs show the thread id; one pane still carries a record of it.
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101"}
	title := sid[:29] + "... | repo"
	w := world(
		paneSpec{agent: "codex", title: title, path: "/repo", opts: opts},
		paneSpec{agent: "codex", title: title, path: "/repo"},
	)
	if r := resolveWith(w, threads{}, Request{Agent: proc.Codex, SessionID: sid, Cwd: "/repo", PID: 902}); r.Evidence != Unbound {
		t.Fatalf("ambiguous title id fell through to %+v", r)
	}
}

func TestOwnRecordSurvivesMisleadingTitles(t *testing.T) {
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101", "state": "idle"}
	for name, c := range map[string]struct {
		title, path string
		idx         threads
	}{
		// A worktree directory named like a thread id is the project item.
		"uuid project": {"⠋ Fix parser | " + other, "/w/" + other, threads{sid: "Fix parser"}},
		// The thread was named moments ago; the index still lacks it, and an
		// older thread carries the same name.
		"index behind title": {"⠋ Fix tests | repo", "/repo", threads{other: "Fix tests"}},
		// The title shows a shorter prefix of this thread's own id.
		"short own id": {sid[:13] + "... | repo", "/repo", threads{}},
	} {
		w := world(paneSpec{agent: "codex", title: c.title, path: c.path, opts: opts})
		w.Threads = c.idx
		if _, ok := w.Record(w.Panes[0]); !ok {
			t.Errorf("%s: own record judged stale", name)
		}
	}
}

func TestForegroundFollowsTheTerminal(t *testing.T) {
	// zsh 100 runs claude 101 in its own process group.
	w := world(paneSpec{agent: "claude"})
	set := func(tpgid int) {
		w.Proc = proc.NewTable([]proc.Process{
			{PID: 100, PPID: 1, PGID: 100, TPGID: tpgid, Comm: "zsh"},
			{PID: 101, PPID: 100, PGID: 101, TPGID: tpgid, Comm: "claude", Args: "claude"},
		})
	}
	set(101)
	if !w.Foreground(w.Panes[0]) {
		t.Fatal("agent holding the terminal is not foreground")
	}
	// Suspended: the shell took the terminal back; the agent still exists.
	set(100)
	if w.Foreground(w.Panes[0]) {
		t.Fatal("suspended agent counted as foreground")
	}
}

func TestProjectItemIsLocatedByPosition(t *testing.T) {
	// Codex labels the project by its repository root, truncated to 24
	// characters, so the label need not be the pane directory's name.
	for name, c := range map[string]struct{ title, path string }{
		"repository subdirectory": {"Fix tests | my-project", "/work/my-project/src"},
		"long project name":       {"Fix tests | an-exceedingly-long-proj...", "/work/an-exceedingly-long-project-name"},
		"thread in another repo":  {"Fix tests | elsewhere", "/work/my-project"},
	} {
		label := readTitle(c.title).project
		idx := threads{sid: label, other: "Fix tests"}
		opts := map[string]string{"agent": "codex", "session": sid, "pid": "101", "state": "idle"}
		w := world(paneSpec{agent: "codex", title: c.title, path: c.path, opts: opts})
		w.Threads = idx
		// The pane shows other: sid's record, named like the project, is stale.
		if _, ok := w.Record(w.Panes[0]); ok {
			t.Errorf("%s: record of a thread named like the project accepted", name)
		}
		w = world(paneSpec{agent: "codex", title: c.title, path: c.path})
		if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: sid, Cwd: c.path, PID: 902}); r.Evidence != Unbound {
			t.Errorf("%s: project label bound as a thread name: %+v", name, r)
		}
		if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: other, Cwd: c.path, PID: 902}); r.Evidence != TitleName {
			t.Errorf("%s: displayed thread not bound: %+v", name, r)
		}
	}
}

func TestLongThreadNamesMatchTheirDisplay(t *testing.T) {
	long := "Fix every integration test in the frontend workspace before release"
	title := "Fix every integration test in the frontend wo... | repo"
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101", "state": "idle"}
	idx := threads{sid: "Original task", other: long}
	w := world(paneSpec{agent: "codex", title: title, path: "/repo", opts: opts})
	w.Threads = idx
	if _, ok := w.Record(w.Panes[0]); ok {
		t.Fatal("record survived a switch to a thread with a truncated name")
	}
	w = world(paneSpec{agent: "codex", title: title, path: "/repo"})
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: other, Cwd: "/repo", PID: 902}); r.Evidence != TitleName {
		t.Fatalf("truncated name not bound: %+v", r)
	}
	// Two names that truncate alike identify neither thread.
	third := "019a2b3c-7777-7f60-8a9b-0c1d2e3f4a5b"
	idx[third] = long[:45] + " and the backend too"
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: other, Cwd: "/repo", PID: 902}); r.Evidence != Unbound {
		t.Fatalf("shared display bound: %+v", r)
	}
	// It still shows that sid is not on screen.
	w = world(paneSpec{agent: "codex", title: title, path: "/repo", opts: opts})
	w.Threads = idx
	if _, ok := w.Record(w.Panes[0]); ok {
		t.Fatal("record survived a title shared by two other threads")
	}
}

func TestThreadNamesMayContainTheSeparator(t *testing.T) {
	idx := threads{sid: "Old thread", other: "Parse a | b"}
	opts := map[string]string{"agent": "codex", "session": sid, "pid": "101", "state": "idle"}
	w := world(paneSpec{agent: "codex", title: "⠋ Parse a | b | repo", path: "/repo", opts: opts})
	w.Threads = idx
	if _, ok := w.Record(w.Panes[0]); ok {
		t.Fatal("record survived a switch to a name with the separator")
	}
	w = world(paneSpec{agent: "codex", title: "Parse a | b | repo", path: "/repo"})
	if r := resolveWith(w, idx, Request{Agent: proc.Codex, SessionID: other, Cwd: "/repo", PID: 902}); r.Evidence != TitleName {
		t.Fatalf("name with the separator not bound: %+v", r)
	}
}
