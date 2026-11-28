package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

// The fake runs as acme, an agent tmag does not know that reports itself
// through tmag hook, and as zeta, one that adapted nothing at all.
const (
	declared = "acme"
	unaware  = "zeta"
)

// A declared agent reports itself under its program's name: its pane shows
// its session by process proof, it takes prompts and interrupts through its
// inbox, gives the reply that ended a turn, and a follower of the watch
// sees it as tmag lists it.
func TestADeclaredAgentReportsItself(t *testing.T) {
	h := start(t)
	f := h.follow()
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": filepath.Join(h.dir, "a"), "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "300"}, filepath.Join(bin, declared))
	p, s := h.waitPane(pane, "idle", "process")
	if s.Agent != declared || p.Agent != declared || s.Runtime != "terminal" || !slices.Equal(s.Capabilities, []string{"send", "interrupt"}) || s.Transcript != nil {
		t.Fatalf("pane %+v shows %+v", p, s)
	}
	f.agrees(h)

	// A person's prompt takes its inbox, idle as well, and its turn ends
	// with the reply the agent gives tmag.
	r := h.tmag("first line\nreview the diff", "send", sidA[:8], "--wait", "--json", "-")
	if r.code != 0 || !strings.Contains(r.out, `"state":"idle"`) {
		t.Fatalf("send --wait: %d %s", r.code, r.out)
	}
	if i := slices.IndexFunc(h.events(sidA), func(e eventlog.Event) bool { return e.Event == eventlog.Send }); i < 0 || h.events(sidA)[i].Via != "inbox" {
		t.Fatalf("the person's prompt did not take the inbox: %+v", h.events(sidA))
	}
	if r := h.tmag("", "read", sidA[:8], "--last", "--json"); r.code != 0 || !strings.Contains(r.out, `"turn":"t1"`) || !strings.Contains(r.out, `"text":"answered: review the diff"`) {
		t.Fatalf("read --last: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "read", sidA[:8], "--turns", "3", "--json"); r.code != 0 || !strings.Contains(r.out, `"text":"first line\nreview the diff"`) {
		t.Fatalf("read --turns: %d %s", r.code, r.out)
	}
	if got, want := h.eventNames(sidA), "SessionStart:idle Attach:idle Send: UserPromptSubmit:working Stop:idle"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	f.agrees(h)

	// No channel tmag has seen work carries an answer to it.
	if r := h.tmag("", "answer", sidA[:8], "r1", "allow"); r.code == 0 {
		t.Fatalf("answer: %d %s", r.code, r.out)
	}

	// Its inbox stops a turn, which the agent then reports over.
	if r := h.tmag("", "send", sidA[:8], "sleep"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "working")
	if r := h.tmag("", "read", sidA[:8], "--last"); r.code != 3 || !strings.Contains(r.out, "refused (busy)") {
		t.Fatalf("read --last while working: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "interrupt", sidA[:8], "--json"); r.code != 0 || !strings.Contains(r.out, `"via":"inbox"`) {
		t.Fatalf("interrupt: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "idle")
	if got := h.eventNames(sidA); !strings.HasSuffix(got, "Send: UserPromptSubmit:working Halt: Interrupt:idle") {
		t.Fatalf("events = %s", got)
	}
	f.agrees(h)

	h.tmux("send-keys", "-t", pane, "C-d")
	h.eventually("session end", func() bool { return strings.HasSuffix(h.eventNames(sidA), "SessionEnd: Detach:") })
	f.agrees(h)
}

// A hook that names a program it does not run under records nothing: a
// declared agent is only what the process table proves.
func TestADeclaredAgentIsOnlyWhatItsProcessProves(t *testing.T) {
	h := start(t)
	b, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": sidB, "cwd": h.dir})
	if r := h.tmag(string(b), "hook", declared); r.code != 0 || r.out != "" {
		t.Fatalf("hook: %d %q", r.code, r.out)
	}
	if _, ok := h.sessionOf(sidB); ok {
		t.Fatal("a hook no process of its program runs recorded a session")
	}
	if log, _ := os.ReadFile(filepath.Join(h.state, "hook.log")); !strings.Contains(string(log), " "+declared+" failed\n") {
		t.Fatalf("hook.log = %q", log)
	}
	if r := h.tmag(string(b), "hook", "bash"); r.code != 0 || !strings.Contains(r.out, "usage: tmag hook <claude|codex|pi|omp|PROGRAM>") {
		t.Fatalf("hook bash: %d %q", r.code, r.out)
	}
}

// A declared session stays with the process that reported it: a second
// process of the program reporting it records nothing while the first runs,
// and takes it over once it is gone, keeping none of its inbox. A session
// that serves a program is asked for nothing.
func TestADeclaredSessionStaysWithItsProcess(t *testing.T) {
	h := start(t)
	f := h.follow()
	outA, outB := filepath.Join(h.dir, "a"), filepath.Join(h.dir, "b")
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": outA, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "300"}, filepath.Join(bin, declared))
	h.waitPane(a, "idle", "process")
	b := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "300"}, filepath.Join(bin, declared))
	h.eventually("the second process's hook to fail", func() bool {
		log, _ := os.ReadFile(filepath.Join(h.state, "hook.log"))
		return strings.Contains(string(log), " "+declared+" failed\n")
	})
	if s, _ := h.sessionOf(sidA); len(s.Panes) != 1 || s.Panes[0].ID != a {
		t.Fatalf("the session shows in %+v, not only in %s", s.Panes, a)
	}
	if r := h.tmag("", "send", sidA[:8], "--wait", "hello"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	lastReply(t, h, sidA, "hello")
	f.agrees(h)

	// The first process ends without SessionEnd; the second reports the
	// session again and takes it, with no inbox of its own.
	h.tmux("kill-pane", "-t", a)
	h.eventually("the first process to be gone", func() bool {
		_, ok := h.sessionOf(sidA)
		return !ok
	})
	h.tmux("send-keys", "-t", b, "hi", "Enter")
	h.waitSession(sidA, "working")
	h.waitPane(b, "idle", "process")
	if r := h.tmag("", "send", sidA[:8], "again"); r.code != 3 || !strings.Contains(r.out, "refused (unbound)") {
		t.Fatalf("a send to the process that took the session over: %d %s", r.code, r.out)
	}
	f.agrees(h)

	// A session that serves a program takes no verb and gives no turns.
	h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_MODE": "rpc"}, filepath.Join(bin, declared))
	h.eventually("the serving session", func() bool {
		s, ok := h.sessionOf(sidC)
		return ok && s.Runtime == "service" && len(s.Panes) == 0
	})
	for _, args := range [][]string{{"read", sidC[:8], "--turns", "1"}, {"read", sidC[:8], "--last"}, {"send", sidC[:8], "hi"}} {
		if r := h.tmag("", args...); r.code != 3 || !strings.Contains(r.out, "refused (unsupported)") {
			t.Fatalf("%v: %d %s", args, r.code, r.out)
		}
	}
}

// A declared agent and Claude Code ask each other as any two agents do:
// tmag writes who asked, records what is owed and settles it with the
// reply, and attests no permission class for the declared agent.
func TestADeclaredAgentWorksWithClaude(t *testing.T) {
	h := start(t)
	f := h.follow()
	outA, outB := filepath.Join(h.dir, "a"), filepath.Join(h.dir, "b")
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": outA, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "300", "FAKE_PERMISSION_MODE": "bypassPermissions"}, filepath.Join(bin, "claude"))
	b := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "300", "FAKE_PERMISSION_MODE": "bypassPermissions"}, filepath.Join(bin, declared))
	h.waitPane(a, "idle", "process")
	h.waitPane(b, "idle", "process")

	// Claude asks the declared agent for a reply later.
	typed(t, h, a, sidA, "tmag send "+sidB[:8]+" --reply review the parser")
	readEventually(t, outA, "tool 0 sent to "+sidB[:8]+" (inbox)")
	h.waitSession(sidB, "idle")
	got := owes(t, h, sidB)
	if len(got) != 1 || got[0].Kind != eventlog.OweReply || got[0].To.Session != sidA {
		t.Fatalf("the declared agent owes %+v", got)
	}
	r := h.tmag("", "read", sidB[:8], "--turns", "1", "--json")
	if r.code != 0 || !strings.Contains(r.out, `"text":"[tmag] From claude `+sidA+`, another agent. When done, send your answer on stdin to: tmag reply `+sidA+` -\nreview the parser"`) {
		t.Fatalf("the declared agent was given: %d %s", r.code, r.out)
	}

	// Its reply reaches Claude in Claude's envelope, under its own name and
	// with no class: Claude, which bypasses its prompts, holds it.
	typed(t, h, b, sidB, "tmag reply "+sidA+" all done")
	readEventually(t, outB, "tool 0 replied to "+sidA[:8])
	readEventually(t, outA, "inbox "+declared+" "+sidB[:8]+" via tmag session="+sidA+"\n")
	if got := owes(t, h, sidB); got != nil {
		t.Fatalf("the reply left %+v owed", got)
	}
	var reply eventlog.Event
	for _, e := range h.events(sidA) {
		if e.Event == eventlog.Send {
			reply = e
		}
	}
	if reply.By == nil || *reply.By != (eventlog.Actor{Agent: declared, Session: sidB}) || reply.From == nil || reply.From.ID != b {
		t.Fatalf("the reply's Send event = %+v", reply)
	}

	// The declared agent's list knows itself and the session it owes nothing
	// now; a person's shell is no agent.
	typed(t, h, b, sidB, "tmag list --json")
	readEventually(t, outB, `"by":{"agent":"`+declared+`","session":"`+sidB+`"}`)
	f.agrees(h)
}

// An agent that adapted nothing is the person's unless the person counts its
// program as an agent: then what it runs is an agent's, refused the person's
// keys and given tmag's first line, whose session tmag cannot tell.
func TestTheProgramsThePersonCountsAsAgents(t *testing.T) {
	h := start(t)
	outB, outZ := filepath.Join(h.dir, "b"), filepath.Join(h.dir, "z")
	b := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	z := h.spawn(h.dir, map[string]string{"FAKE_OUT": outZ}, filepath.Join(bin, unaware))
	h.waitPane(b, "idle", "process")
	readEventually(t, outZ) // the unaware agent runs, reporting nothing
	screen := "5:" + strings.Repeat("0", 64)
	run := func(n int, line string) string {
		t.Helper()
		h.tmux("send-keys", "-t", z, line, "Enter")
		var got string
		h.eventually(unaware+" runs "+line, func() bool {
			b, _ := os.ReadFile(outZ)
			var tools []string
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "tool ") {
					tools = append(tools, l)
				}
			}
			if len(tools) <= n {
				return false
			}
			got = tools[n]
			return true
		})
		return got
	}

	if got := run(0, "tmag keys "+h.target(b)+" --screen "+screen+" Enter"); strings.Contains(got, "refused (agent)") {
		t.Fatalf("keys of an agent nobody counts: %s", got)
	}
	if got := run(1, "tmag send "+sidB[:8]+" --wait hello"); !strings.HasPrefix(got, "tool 0 ") {
		t.Fatalf("send of an agent nobody counts: %s", got)
	}
	readEventually(t, outB, "hello\n")
	if b, _ := os.ReadFile(outB); strings.Contains(string(b), "[tmag]") {
		t.Fatalf("a person's prompt got tmag's line: %q", b)
	}

	h.tmux("set-option", "-g", "@tmag-agents", "aider, "+unaware)
	h.waitSession(sidB, "idle")
	if got := run(2, "tmag keys "+h.target(b)+" --screen "+screen+" Enter"); !strings.HasPrefix(got, "tool 3 ") || !strings.Contains(got, "refused (agent)") {
		t.Fatalf("keys of a counted agent: %s", got)
	}
	if got := run(3, "tmag send "+sidB[:8]+" --wait again"); !strings.HasPrefix(got, "tool 0 ") {
		t.Fatalf("send of a counted agent: %s", got)
	}
	readEventually(t, outB, "[tmag] From "+unaware+", another agent. Answer in your final message.\nagain\n")
	var send eventlog.Event
	for _, e := range h.events(sidB) {
		if e.Event == eventlog.Send {
			send = e
		}
	}
	if send.By == nil || *send.By != (eventlog.Actor{Agent: unaware}) || send.From == nil || send.From.ID != z {
		t.Fatalf("the counted agent's Send event = %+v", send)
	}
	// Its session is unknown, so nothing it could be owed reaches it.
	if got := run(4, "tmag send "+sidB[:8]+" --reply more"); !strings.Contains(got, "only an agent") {
		t.Fatalf("send --reply of a counted agent: %s", got)
	}
}

// The person starts a declared agent from a template, as any agent: tmag
// waits until its pane shows its session and gives it the prompt.
func TestAStartTemplateRunsADeclaredAgent(t *testing.T) {
	h := start(t)
	f := h.follow()
	h.tmux("set-option", "-g", "@tmag-start-ac", filepath.Join(bin, declared))
	startable(h, map[string]string{"FAKE_SESSION": sidC, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "300", "FAKE_OUT": filepath.Join(h.dir, "c")})
	out := person(h, `t="$TMAG_BIN"
"$t" start `+declared+` hi >"$OUT" 2>&1; echo "exit $?" >>"$OUT"
"$t" start ac hello >>"$OUT" 2>&1`)
	readEventually(t, out,
		"tmag start: usage: no @tmag-start-"+declared+" is set on this tmux server\nexit 2",
		"started "+declared+" "+sidC[:8]+" in %", "sent to "+sidC[:8]+" (inbox)", "exit 0")
	if s := h.waitSession(sidC, "idle"); s.Agent != declared || len(s.Panes) != 1 {
		t.Fatalf("the started session: %+v", s)
	}
	if ev := startEvent(t, h, sidC); ev.Template != "ac" || ev.Agent != declared {
		t.Fatalf("Start event = %+v", ev)
	}
	lastReply(t, h, sidC, "hello")
	f.agrees(h)
}
