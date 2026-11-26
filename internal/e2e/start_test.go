package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// startable gives the windows tmag start opens in the private server env
// for the fake agent they run and the session it shows.
func startable(h *harness, env map[string]string) {
	env["TMAG_BIN"] = filepath.Join(bin, "tmag")
	for k, v := range env {
		h.tmux("set-environment", "-g", k, v)
	}
}

// person runs script with sh in a pane of its own, as a person at a shell
// would, and returns the file its output and exit status go to.
func person(h *harness, script string, args ...string) string {
	out := filepath.Join(h.dir, "person-"+strings.ReplaceAll(h.t.Name(), "/", "-"))
	h.spawn(h.dir, map[string]string{"OUT": out}, append([]string{"/bin/sh", "-c", script + `; echo "exit $?" >>"$OUT"; exec sleep 600`, "sh"}, args...)...)
	return out
}

// startEvent is the Start event of a session.
func startEvent(t *testing.T, h *harness, sid string) eventlog.Event {
	t.Helper()
	events := h.events(sid)
	i := slices.IndexFunc(events, func(e eventlog.Event) bool { return e.Event == eventlog.Start })
	if i < 0 {
		t.Fatalf("no Start event for %s: %+v", sid, events)
	}
	return events[i]
}

// An agent starts a peer in a window beside its own and asks it: the peer
// owes the answer, and the Start event says who started it.
func TestAnAgentStartsAPeerAndAsksIt(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA)
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "500", "FAKE_OUT": filepath.Join(h.dir, "c")})

	typed(t, h, panes[0], sidA, "tmag start claude --reply --timeout 20s -- review the parser")
	readEventually(t, outs[0], "tool 0 started claude "+sidC[:8]+" in %", "sent to "+sidC[:8]+" (inbox)")
	h.waitSession(sidC, "idle")
	got := owes(t, h, sidC)
	if len(got) != 1 || got[0].Kind != eventlog.OweReply || got[0].To.Session != sidA {
		t.Fatalf("the started peer owes %+v", got)
	}
	ev := startEvent(t, h, sidC)
	starter := eventlog.Actor{Agent: "claude", Session: sidA}
	if ev.By == nil || ev.By.Session != sidA || ev.Pane == nil || ev.Cwd != resolved(t, h.dir) || ev.Template != "" || ev.State != "" {
		t.Fatalf("Start event = %+v", ev)
	}
	// The StartedBy event after it reports the session with its starter.
	if !slices.ContainsFunc(h.events(sidC), func(e eventlog.Event) bool {
		return e.Event == eventlog.StartedBy && e.Seq > ev.Seq && e.State != "" && e.StartedBy != nil && *e.StartedBy == starter
	}) {
		t.Fatalf("no StartedBy event reports the starter after the Start: %+v", h.events(sidC))
	}
	if s, _ := h.sessionOf(sidC); len(s.Panes) != 1 || s.Panes[0].ID != ev.Pane.ID || s.StartedBy == nil || *s.StartedBy != starter {
		t.Fatalf("the started session shows in %+v, started by %+v; Start names %+v", s.Panes, s.StartedBy, ev.Pane)
	}
	lastReply(t, h, sidC, "review the parser")
	f.agrees(h)
}

// A person starts the agent a template runs, in a worktree of its own, and
// prompts it as send does; what a start may run is the person's alone.
func TestAPersonStartsATemplateInAWorktree(t *testing.T) {
	h := start(t)
	f := h.follow()
	repo := filepath.Join(h.dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	h.tmux("set-option", "-g", "@tmag-start-fake", filepath.Join(bin, "claude"))
	h.tmux("set-option", "-g", "@tmag-start-shell", "/bin/sh -i")
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "500", "FAKE_OUT": filepath.Join(h.dir, "c")})

	out := person(h, `t="$TMAG_BIN"
"$t" start Bad >"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start unset >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start shell >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start fake --wait >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start fake --dir "$1" --worktree feat/x hello >>"$OUT" 2>&1`, repo)
	readEventually(t, out,
		"tmag start: usage: a template is named by lowercase letters, digits, - and _\nexit 2",
		"tmag start: usage: no @tmag-start-unset is set on this tmux server\nexit 2",
		"tmag start: usage: @tmag-start-shell runs /bin/sh, which is none of claude, codex, pi, omp\nexit 2",
		"tmag start: --wait and --reply ask about a prompt: give one, or - for stdin\nexit 2",
		"started claude "+sidC[:8]+" in %", "sent to "+sidC[:8]+" (pane)", "exit 0")
	tree := resolved(t, filepath.Join(repo, ".worktrees", "feat", "x"))
	if b, err := exec.Command("git", "-C", tree, "rev-parse", "--abbrev-ref", "HEAD").Output(); err != nil || strings.TrimSpace(string(b)) != "feat/x" {
		t.Fatalf("worktree %s on %q: %v", tree, b, err)
	}
	if s := h.waitSession(sidC, "idle"); s.Cwd != tree {
		t.Fatalf("the started session runs in %s, not its worktree", s.Cwd)
	}
	if ev := startEvent(t, h, sidC); ev.By != nil || ev.StartedBy != nil || ev.Template != "fake" || ev.Cwd != tree {
		t.Fatalf("Start event = %+v", ev)
	}
	if slices.ContainsFunc(h.events(sidC), func(e eventlog.Event) bool { return e.Event == eventlog.StartedBy }) {
		t.Fatal("a person's start reports a starter")
	}
	if s, _ := h.sessionOf(sidC); s.StartedBy != nil {
		t.Fatalf("a person's start names %+v as the starter", s.StartedBy)
	}
	lastReply(t, h, sidC, "hello")
	if got := owes(t, h, sidC); got != nil {
		t.Fatalf("a person's prompt is owed %+v", got)
	}
	f.agrees(h)
}

// A start names the window it opens, so the person and the agent that
// started it tell many peers apart, and the agent addresses one by name; a
// name that would not address one window is refused before a window opens.
func TestAStartNamesItsWindow(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA)
	outC := filepath.Join(h.dir, "c")
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "500", "FAKE_OUT": outC})

	typed(t, h, panes[0], sidA, "tmag start claude --name rev-1 --reply --timeout 20s --json -- stand by")
	// The start's own result is the session as recorded, its starter too.
	readEventually(t, outs[0], `"id":"`+sidC+`"`, `"started_by":{"agent":"claude","session":"`+sidA+`"}`)
	s := h.waitSession(sidC, "idle")
	if len(s.Panes) != 1 {
		t.Fatalf("the started session shows in %+v", s.Panes)
	}
	if r := h.tmag("", "list", "--json"); !strings.Contains(r.out, `"id":"`+s.Panes[0].ID+`","target":"main:`) || !strings.Contains(r.out, `"window":"rev-1"`) {
		t.Fatalf("list --json does not name the started agent's window: %s", r.out)
	}
	// After a turn of the agent's, so tmux would have renamed it by now.
	if got := h.tmux("display-message", "-p", "-t", s.Panes[0].ID, "#{window_name}"); got != "rev-1" {
		t.Fatalf("the started agent's window is named %q", got)
	}
	windows := func() int { return strings.Count(h.tmux("list-windows", "-a", "-F", "#{window_id}"), "@") }
	before := windows()
	for name, why := range map[string]string{
		"rev-1":  "usage: a window of tmux session main is named rev-1 already, and a name names one window",
		"7":      "usage: a window is named by up to 32 letters, digits, - and _, starting with a letter or digit, and not only by 0-9, a-f and -, which a session id is made of",
		"beef-1": "usage: a window is named by up to 32 letters",
		"a.b":    "usage: a window is named by up to 32 letters",
		"-x":     "usage: a window is named by up to 32 letters",
		"":       "--name names the window: give a name, or leave --name out",
	} {
		typed(t, h, panes[0], sidA, "tmag start claude --name="+name+" -- hi")
		readEventually(t, outs[0], "tool 2 tmag start: "+why)
	}
	if got := windows(); got != before {
		t.Fatalf("refused starts opened %d windows", got-before)
	}
	typed(t, h, panes[0], sidA, "tmag send rev-1 -- hello there")
	readEventually(t, outs[0], "tool 0 sent to "+sidC[:8])
	lastReply(t, h, sidC, "hello there")
	f.agrees(h)
}

// Starts at once under one name open one window: tmux decides that no window
// has the name and opens it in one command, and the others are refused.
func TestStartsAtOnceTakeANameOnce(t *testing.T) {
	h := start(t)
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1"})
	out := person(h, `for i in 1 2 3 4 5 6 7 8; do "$TMAG_BIN" start claude --name same --timeout 10s >"$OUT.$i" 2>&1 & done; wait`)
	readEventually(t, out, "exit 0")
	named := 0
	for _, name := range strings.Split(h.tmux("list-windows", "-a", "-F", "#{window_name}"), "\n") {
		if name == "same" {
			named++
		}
	}
	refused := 0
	for i := 1; i <= 8; i++ {
		b, _ := os.ReadFile(out + "." + strconv.Itoa(i))
		if strings.Contains(string(b), "usage: a window of tmux session main is named same already") {
			refused++
		}
	}
	if named != 1 || refused != 7 {
		t.Fatalf("8 starts at once under one name opened %d windows of it, and %d were refused", named, refused)
	}
}

// A start whose window shows a session tmag listed before it opened, as a
// template that resumes one does, did not bring that session up: the agent
// session that did stays its starter.
func TestAStartOfAShownSessionKeepsItsStarter(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB)
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "300"})
	typed(t, h, panes[0], sidA, "tmag start claude --timeout 20s")
	readEventually(t, outs[0], "tool 0 started claude "+sidC[:8]+" in %")
	h.waitSession(sidC, "idle")
	typed(t, h, panes[1], sidB, "tmag start claude --timeout 20s")
	readEventually(t, outs[1], "tool 0 started claude "+sidC[:8]+" in %")
	s, _ := h.sessionOf(sidC)
	if s.StartedBy == nil || s.StartedBy.Session != sidA {
		t.Fatalf("a second start of %s made its starter %+v", sidC[:8], s.StartedBy)
	}
	f.agrees(h)
}

// A session whose record tmag cannot read right before a start opens its
// window may still run, as this one does, its record spoiled while the
// start adds its worktree: a window that then shows it was not brought up
// by the start, which claims nothing.
func TestAStartOfAnUnreadableSessionClaimsNothing(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB)
	repo := filepath.Join(h.dir, "repo")
	hooks := filepath.Join(h.dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	spoil := "#!/bin/sh\nprintf '{' >" + tmux.Quote(filepath.Join(h.state, "sessions", "claude", sidB+".json")) + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(spoil), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "config", "core.hooksPath", hooks},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	startable(h, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "300"})
	typed(t, h, panes[0], sidA, "tmag start claude --dir "+repo+" --worktree spoiled --timeout 20s")
	readEventually(t, outs[0], "tool 0 started claude "+sidB[:8]+" in %")
	if s, _ := h.sessionOf(sidB); s.StartedBy != nil {
		t.Fatalf("a start claimed a session that ran before it: started by %+v", s.StartedBy)
	}
	f.agrees(h)
}

// A session another pane shows while tmux opens a start's window, the start
// held there as a busy machine may hold it, ran before the window: when the
// window then shows it, the start claims nothing.
func TestAStartOfASessionShownWhileItsWindowOpensClaimsNothing(t *testing.T) {
	h := start(t)
	f := h.follow()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	held, go_ := filepath.Join(h.dir, "held"), filepath.Join(h.dir, "go")
	wrap := filepath.Join(h.dir, "wrap")
	if err := os.Mkdir(wrap, 0o755); err != nil {
		t.Fatal(err)
	}
	// The hub's tmux waits, at the command that opens a named window, until
	// the test lets it go.
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$4\" = if-shell ]; then\n echo held >%s\n while [ ! -e %s ]; do sleep .01; done\nfi\nexec %s \"$@\"\n",
		tmux.Quote(held), tmux.Quote(go_), tmux.Quote(real))
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(h.dir, "hub")
	hub := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out},
		"sh", "-c", `PATH="$1:$PATH" exec "$2"`, "sh", wrap, filepath.Join(bin, "claude"))
	h.waitPane(hub, "idle", "process")
	startable(h, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"})
	h.tmux("send-keys", "-t", hub, "tmag start claude --name late --timeout 20s", "Enter")
	readEventually(t, held, "held")
	older := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(older, "idle", "process")
	if err := os.WriteFile(go_, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readEventually(t, out, "tool 0 started claude "+sidB[:8]+" in %")
	if s, _ := h.sessionOf(sidB); s.StartedBy != nil || slices.ContainsFunc(h.events(sidB), func(e eventlog.Event) bool { return e.Event == eventlog.StartedBy }) {
		t.Fatalf("a start claimed a session another pane showed while its window opened: %+v", s.StartedBy)
	}
	f.agrees(h)
}

// An agent whose pane shows no session in time is left in its window, and
// tmag says where to look.
func TestAStartThatShowsNoSessionLeavesItsWindow(t *testing.T) {
	h := start(t)
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_OUT": filepath.Join(h.dir, "c")})
	out := person(h, `"$TMAG_BIN" start claude --timeout 2s --json >"$OUT" 2>&1`)
	readEventually(t, out, "which shows no session tmag can prove yet", `"pane":{"server":"`, `"id":"%`, "next: its window stays: see what holds it with tmag read %", "exit 4")
	panes := h.tmux("list-panes", "-a", "-F", "#{pane_current_command}")
	if strings.Count(panes, "claude") != 1 {
		t.Fatalf("the started agent's window is gone: %q", panes)
	}
}

// Codex shows its session only from its first prompt: tmag gives it one of
// its own with its command, then, once that prompt's turn ended, its task
// through send, which it may answer at once, as the ask it owes is opened
// with the task. Its pane may show its session before that turn starts.
func TestCodexStartsOnAFirstPromptOfTmagsThenTakesItsTask(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA)
	outC := filepath.Join(h.dir, "c")
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "500", "FAKE_FIRST_MS": "1500", "FAKE_OUT": outC})

	typed(t, h, panes[0], sidA, "tmag start codex --reply --timeout 20s -- tmag reply "+sidA+" at once")
	readEventually(t, outs[0], "tool 0 started codex "+sidC[:8]+" in %", "sent to "+sidC[:8]+" (pane)")
	readEventually(t, outC, "tmag started you in this tmux window. Reply ready, and wait for the next prompt.",
		"[tmag] From claude "+sidA+", another agent. When done, send your answer on stdin to: tmag reply "+sidA+" -\ntmag reply "+sidA+" at once",
		"tool 0 replied to "+sidA[:8])
	if b, _ := os.ReadFile(outC); strings.Index(string(b), "[tmag] From") < strings.Index(string(b), "tmag started you") {
		t.Fatalf("Codex took its task before tmag's first prompt:\n%s", b)
	}
	h.waitSession(sidC, "idle")
	if got := owes(t, h, sidC); got != nil {
		t.Fatalf("Codex still owes %+v after its reply", got)
	}
	readEventually(t, outs[0], "message [tmag] Reply from codex "+sidC+", another agent, to your prompt. Do not reply to it.\nat once")
	f.agrees(h)
}

// What a start would leave wrong is refused before a window opens: a
// worktree git does not list, or lists on another branch; a person's ask for
// a reply; a first line tmag did not write.
func TestAStartIsRefusedBeforeItsWindowOpens(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	gitIn := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".worktrees", "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn("init", "-q")
	gitIn("commit", "-q", "--allow-empty", "-m", "init")
	gitIn("worktree", "add", "-q", "-b", "other", filepath.Join(repo, ".worktrees", "elsewhere"))
	if err := os.MkdirAll(filepath.Join(h.dir, "loose"), 0o755); err != nil {
		t.Fatal(err)
	}
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_OUT": filepath.Join(h.dir, "c")})
	h.tmux("new-window", "-d", "-t", "=main:", "-n", "taken", "sleep 600")
	windows := func() int { return len(strings.Split(h.tmux("list-windows", "-a"), "\n")) }
	out := person(h, `t="$TMAG_BIN"
"$t" start claude --dir "$1" --worktree plain >"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start claude --dir "$1" --worktree elsewhere >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start claude --dir "$2" --worktree any >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start claude --name taken --dir "$1" --worktree feat/t >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start claude --reply hello >>"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start claude "$(printf '\001[tmag] From claude x')" >>"$OUT" 2>&1`, repo, filepath.Join(h.dir, "loose"))
	readEventually(t, out,
		"exists and is no worktree of "+resolved(t, repo)+"\nexit 2",
		"/.worktrees/elsewhere is a worktree, not on elsewhere\nexit 2",
		"is in no git repository\nexit 2",
		"usage: a window of tmux session main is named taken already, and a name names one window\nexit 2",
		"only an agent whose session tmag proves sends or asks for a reply",
		"a prompt may not start with [tmag]",
		"exit 2")
	if n := windows(); n != 3 {
		t.Fatalf("refused starts opened windows: %s", h.tmux("list-windows", "-a", "-F", "#{window_index} #{pane_start_command}"))
	}
	// A name taken before the worktree is added adds none.
	if b, _ := exec.Command("git", "-C", repo, "branch", "--list", "feat/t").Output(); len(b) != 0 {
		t.Fatalf("a refused start added the branch feat/t")
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feat", "t")); err == nil {
		t.Fatal("a refused start added a worktree")
	}
}

// tmux expands a start directory as a format, and ends a command at an
// argument that ends in ";": tmag gives it the directory as a literal, so a
// path holding #{…} names no other directory, named window or not.
func TestAStartTakesItsDirectoryLiterally(t *testing.T) {
	h := start(t)
	literal := filepath.Join(h.dir, "part #{session_name} #S;")
	for _, d := range []string{literal, filepath.Join(h.dir, "part main main;")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"--name=part-z", "--json"} {
		sid := []string{sidB, sidC}[i]
		startable(h, map[string]string{"FAKE_SESSION": sid, "FAKE_HOOKS": "1"})
		// The path goes by a file: tmux would end the person's command at it.
		at := filepath.Join(h.dir, "dir")
		if err := os.WriteFile(at, []byte(literal), 0o600); err != nil {
			t.Fatal(err)
		}
		out := person(h, `"$TMAG_BIN" start claude `+name+` --dir "$(cat "$1")" >"$OUT" 2>&1`, at)
		readEventually(t, out, sid[:8], "exit 0")
		if s := h.waitSession(sid, "idle"); s.Cwd != resolved(t, literal) {
			t.Fatalf("start %s runs its agent in %q, not %q", name, s.Cwd, literal)
		}
		if ev := startEvent(t, h, sid); ev.Cwd != literal {
			t.Fatalf("start %s logs its directory as %q", name, ev.Cwd)
		}
	}
}

// resolved is path as tmux and git name it: on macOS, /var is /private/var.
func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
