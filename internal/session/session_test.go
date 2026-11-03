package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

var t0 = time.Unix(100, 0).UTC()

// play applies events in order, starting from a main-agent turn p1.
func play(t *testing.T, trs ...adapter.Transition) Record {
	t.Helper()
	cur := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Turn: "p1", Since: t0}
	for _, tr := range trs {
		cur, _ = Apply(cur, tr, t0)
	}
	return cur
}

func askFor(agent, tool string) adapter.Transition {
	p := adapter.Prompt{Agent: agent, Tool: tool}
	return adapter.Transition{Action: adapter.Ask, Prompt: p}
}

// reminder is the main agent's permission_prompt notification.
func reminder() adapter.Transition {
	tr := askFor("", "")
	tr.Reminder = true
	return tr
}

func startOf(agent, tool, call string) adapter.Transition {
	return adapter.Transition{Action: adapter.Start, Call: adapter.Call{Agent: agent, Tool: tool, ID: call}}
}

func finishOf(agent, tool, call string) adapter.Transition {
	return adapter.Transition{Action: adapter.Finish, Call: adapter.Call{Agent: agent, Tool: tool, ID: call}}
}

func settleOf(agent string) adapter.Transition {
	return adapter.Transition{Action: adapter.Settle, Call: adapter.Call{Agent: agent}}
}

func spawnOf(agent string) adapter.Transition {
	return adapter.Transition{Action: adapter.Spawn, Call: adapter.Call{Agent: agent}}
}

func reapOf(agent string) adapter.Transition {
	return adapter.Transition{Action: adapter.Reap, Call: adapter.Call{Agent: agent}}
}

func stopWith(tasks ...string) adapter.Transition {
	return adapter.Transition{Action: adapter.Set, State: adapter.Idle, Turn: "p1", Background: &tasks}
}

var (
	codexStop = adapter.Transition{Action: adapter.Set, State: adapter.Idle, Turn: "p1"}
	submit    = adapter.Transition{Action: adapter.Set, State: adapter.Working, Turn: "p2", KeepSubagents: true}
)

// answerOf is a call of tool by agent that ran and finished.
func answerOf(agent, tool string) adapter.Transition { return finishOf(agent, tool, "x-"+agent+tool) }

func is(t *testing.T, rec Record, state, kind string) {
	t.Helper()
	if rec.State() != state || rec.Kind() != kind {
		t.Fatalf("state %s/%s, want %s/%s: %+v", rec.State(), rec.Kind(), state, kind, rec)
	}
}

func TestUnrelatedCompletionKeepsPromptOpen(t *testing.T) {
	// Subagent B waits for Bash approval while subagent A finishes its own Bash.
	is(t, play(t, askFor("B", "Bash"), answerOf("A", "Bash")), adapter.Blocked, adapter.KindPermission)
	is(t, play(t, askFor("B", "Bash"), answerOf("B", "Read")), adapter.Blocked, adapter.KindPermission)
	is(t, play(t, askFor("B", "Bash"), answerOf("B", "Bash")), adapter.Working, "")
}

func TestQueuedPromptsBlockUntilAllAreAnswered(t *testing.T) {
	is(t, play(t, askFor("A", "Bash"), askFor("", "AskUserQuestion"), answerOf("A", "Bash")), adapter.Blocked, adapter.KindQuestion)
	is(t, play(t, askFor("A", "Bash"), askFor("", "AskUserQuestion"), answerOf("A", "Bash"), answerOf("", "AskUserQuestion")), adapter.Working, "")
}

func TestUnnamedPrompt(t *testing.T) {
	// A sandboxed command's network request: only a notification, no
	// PermissionRequest. The command's completion answers it.
	is(t, play(t, reminder()), adapter.Blocked, adapter.KindPermission)
	is(t, play(t, reminder(), answerOf("", "Bash")), adapter.Working, "")
	// The notification may not name the subagent whose command asked.
	is(t, play(t, reminder(), answerOf("X", "Bash")), adapter.Working, "")
	// Repeating a pending PermissionRequest adds nothing.
	is(t, play(t, askFor("A", "Bash"), reminder(), answerOf("A", "Bash")), adapter.Working, "")
	// Outside a turn it is noise.
	idle := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Since: t0}
	if _, changed := Apply(idle, reminder(), t0); changed {
		t.Fatal("notification blocked an idle session")
	}
}

func TestUnnamedPermissionRequestBlocksAnywhere(t *testing.T) {
	// A permission request without a tool name is a prompt even as the first
	// event of a session; the next completion answers it.
	rec, changed := Apply(Record{Agent: proc.Codex, ID: "s1"}, askFor("", ""), t0)
	if !changed {
		t.Fatal("no change")
	}
	is(t, rec, adapter.Blocked, adapter.KindPermission)
	rec, _ = Apply(rec, answerOf("", "shell"), t0)
	is(t, rec, adapter.Working, "")
}

func TestTurnEventsCloseEveryPrompt(t *testing.T) {
	rec := play(t, askFor("A", "Bash"), askFor("B", "Bash"), stopWith())
	is(t, rec, adapter.Idle, "")
	if rec.Pending != nil {
		t.Fatalf("Stop left prompts: %+v", rec)
	}
}

func TestTurnStartConfirmsSubmittedPrompt(t *testing.T) {
	cur := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Sent: time.Unix(200, 0).UTC(), Since: t0}
	idle := adapter.Transition{Action: adapter.Set, State: adapter.Idle}
	if rec, _ := Apply(cur, idle, t0); rec.Sent.IsZero() {
		t.Fatal("idle notification dropped the submitted marker")
	}
	if rec, _ := Apply(cur, submit, t0); !rec.Sent.IsZero() {
		t.Fatal("turn start kept the submitted marker")
	}
}

func TestParallelCallsOfOneToolKeepItsPromptOpen(t *testing.T) {
	// Claude 2.1.317, two parallel Reads, one outside the project. The
	// permission request names no call; the allowed Read finishes first.
	for name, order := range map[string][]adapter.Transition{
		"both started first":              {startOf("", "Read", "r1"), startOf("", "Read", "r2"), askFor("", "Read"), finishOf("", "Read", "r1")},
		"asked before the second started": {startOf("", "Read", "r1"), askFor("", "Read"), startOf("", "Read", "r2"), finishOf("", "Read", "r2")},
	} {
		if rec := play(t, order...); rec.State() != adapter.Blocked || len(rec.Pending) != 1 {
			t.Errorf("%s: the allowed call released the prompt: %+v", name, rec)
		}
		rec := play(t, append(order, finishOf("", "Read", "r2"), finishOf("", "Read", "r1"))...)
		if rec.State() != adapter.Working || rec.Pending != nil || rec.Calls != nil {
			t.Errorf("%s: both calls over, still %+v", name, rec)
		}
	}
}

func TestFinishBeforeStartLeavesNoRunningCall(t *testing.T) {
	// Claude runs PostToolUse hooks of parallel calls concurrently; one may
	// be applied before its PreToolUse.
	rec := play(t, finishOf("", "Read", "r1"), startOf("", "Read", "r1"), askFor("", "Read"), finishOf("", "Read", "r2"))
	is(t, rec, adapter.Working, "")
	if rec.Calls != nil {
		t.Fatalf("a finished call runs again: %+v", rec)
	}
}

func TestSettleClosesTheAgentsPrompts(t *testing.T) {
	// A call denied with Esc reports no PostToolUse; its batch still resolves.
	rec := play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), settleOf("w"))
	is(t, rec, adapter.Working, "")
	if rec.Calls != nil {
		t.Fatalf("calls left: %+v", rec)
	}
	// Another agent's batch says nothing about it.
	is(t, play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), settleOf("")), adapter.Blocked, adapter.KindPermission)
}

func TestClaudeBackgroundWorkerOutlivesTheTurn(t *testing.T) {
	// Claude 2.1.317: a background worker asks, then the main agent stops
	// with the worker still listed as running.
	asked := []adapter.Transition{spawnOf("w"), startOf("w", "Bash", "b1"), askFor("w", "Bash"), askFor("", "Read"), stopWith("w")}
	rec := play(t, asked...)
	is(t, rec, adapter.Blocked, adapter.KindPermission)
	if len(rec.Pending) != 1 || rec.Pending[0].Agent != "w" {
		t.Fatalf("Stop kept the wrong prompts: %+v", rec)
	}
	// Answered: back to background work, not to a turn.
	is(t, play(t, append(asked, finishOf("w", "Bash", "b1"))...), adapter.Working, adapter.KindBackground)
	// A new turn of the main agent keeps it too.
	rec = play(t, append(asked, submit)...)
	is(t, rec, adapter.Blocked, adapter.KindPermission)
	if rec.Turn != "p2" {
		t.Fatalf("turn %q", rec.Turn)
	}
	// A Stop that no longer lists the worker ends it.
	rec = play(t, append(asked, stopWith())...)
	is(t, rec, adapter.Idle, "")
	if rec.Pending != nil || rec.Calls != nil || rec.Background != nil {
		t.Fatalf("final Stop left %+v", rec)
	}
}

func TestCodexSubagentOutlivesTheTurn(t *testing.T) {
	// Codex 0.166: the main agent's Stop arrives before SubagentStart, and
	// lists no background work.
	rec := play(t, codexStop, spawnOf("a1"))
	is(t, rec, adapter.Working, adapter.KindBackground)
	is(t, play(t, codexStop, spawnOf("a1"), askFor("a1", "Bash")), adapter.Blocked, adapter.KindPermission)
	rec = play(t, codexStop, spawnOf("a1"), askFor("a1", "Bash"), reapOf("a1"))
	is(t, rec, adapter.Idle, "")
	if !rec.ShowsSubagent("a1") {
		t.Fatal("a finished subagent is forgotten; the TUI may still show it")
	}
}

func TestSubagentTurnsDoNotReplaceTheMainTurn(t *testing.T) {
	sub := spawnOf("a1")
	sub.Turn = "sub-turn"
	start := startOf("a1", "Bash", "c1")
	start.Turn = "sub-turn"
	if rec := play(t, sub, start); rec.Turn != "p1" {
		t.Fatalf("turn %q, want the main agent's p1", rec.Turn)
	}
}

func TestFreshSessionDropsBackground(t *testing.T) {
	start := adapter.Transition{Action: adapter.Set, State: adapter.Idle, Fresh: true}
	is(t, play(t, codexStop, spawnOf("a1"), start), adapter.Idle, "")
}

func TestCallStartedMidTurnShowsWorking(t *testing.T) {
	// tmag installed while a turn ran: the first event it sees is a call.
	rec, _ := Apply(Record{Agent: proc.Codex, ID: "s1"}, startOf("", "Bash", "c1"), t0)
	is(t, rec, adapter.Working, "")
	rec, _ = Apply(Record{Agent: proc.Codex, ID: "s1"}, startOf("a1", "Bash", "c1"), t0)
	is(t, rec, adapter.Unknown, "")
}

func TestSinceChangesWithTheReportedState(t *testing.T) {
	later := t0.Add(time.Minute)
	rec, changed := Apply(play(t), startOf("", "Read", "r1"), later)
	if !changed || !rec.Since.Equal(t0) {
		t.Fatalf("call bookkeeping moved since: %+v", rec)
	}
	if rec, _ = Apply(rec, askFor("", "Read"), later); !rec.Since.Equal(later) {
		t.Fatalf("blocking did not move since: %+v", rec)
	}
}

func TestStore(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	rec := play(t, askFor("", "Bash"))
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(proc.Claude, "s1")
	if err != nil || !ok || !got.equal(rec) {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	all, err := s.All()
	if err != nil || len(all) != 1 {
		t.Fatalf("All = %+v, %v", all, err)
	}
	if err := s.Delete(proc.Claude, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(proc.Claude, "s1"); ok {
		t.Fatal("deleted session still there")
	}
	for _, id := range []string{"", "../x", "a/b", ".hidden"} {
		if err := s.Put(Record{Agent: proc.Claude, ID: id}); err == nil {
			t.Errorf("Put accepted id %q", id)
		}
	}
	if err := s.Put(Record{Agent: "aider", ID: "s1"}); err == nil {
		t.Error("Put accepted an unknown agent")
	}
	if matches, _ := filepath.Glob(filepath.Join(s.Dir, "sessions", "*", ".tmp-*")); len(matches) > 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
}

func TestSaveLeavesNoChangeUnlogged(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	ctx := context.Background()
	events := func() string {
		all, _, err := log.Since(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range all {
			names = append(names, e.Event+":"+e.State)
		}
		return strings.Join(names, " ")
	}

	rec, _ := Apply(Record{Agent: proc.Claude, ID: "s1"}, adapter.Transition{Action: adapter.Set, State: adapter.Idle}, t0)
	if !rec.Unlogged() {
		t.Fatal("a new session counts as logged")
	}
	rec, err := s.Save(ctx, log, rec, eventlog.Event{Event: "SessionStart"})
	if err != nil || rec.Unlogged() || events() != "SessionStart:idle" {
		t.Fatalf("Save = %+v, %v; events %s", rec, err, events())
	}
	// Bookkeeping alone logs nothing.
	busy, _ := Apply(rec, startOf("", "Read", "r1"), t0)
	if _, err := s.Save(ctx, log, busy, eventlog.Event{Event: "PreToolUse"}); err != nil || events() != "SessionStart:idle" {
		t.Fatalf("bookkeeping logged: %v %s", err, events())
	}

	// A writer that dies after storing a change and before logging it.
	working, _ := Apply(rec, submit, t0)
	working.Change = "UserPromptSubmit"
	if err := s.Put(working); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := s.Get(proc.Claude, "s1")
	settled, ok, err := s.Settle(ctx, log, stored)
	if err != nil || !ok || settled.Unlogged() || events() != "SessionStart:idle UserPromptSubmit:working" {
		t.Fatalf("Settle = %+v, %v, %v; events %s", settled, ok, err, events())
	}
	if again, _, _ := s.Settle(ctx, log, settled); again.Unlogged() || events() != "SessionStart:idle UserPromptSubmit:working" {
		t.Fatalf("a settled record logged again: %s", events())
	}

	// A writer that dies after deciding the end and before logging it.
	settled.Ended = eventlog.End
	if err := s.Put(settled); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = s.Get(proc.Claude, "s1")
	if _, ok, err := s.Settle(ctx, log, stored); ok || err != nil {
		t.Fatalf("an ended session lives on: %v %v", ok, err)
	}
	if _, found, _ := s.Get(proc.Claude, "s1"); found || !strings.HasSuffix(events(), "SessionEnd:") {
		t.Fatalf("end not finished: found %v, events %s", found, events())
	}
}
