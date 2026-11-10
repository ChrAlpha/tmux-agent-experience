package session

import (
	"context"
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

func TestQueuedPromptRunsWithoutItsOwnSubmit(t *testing.T) {
	// Claude reports a prompt queued during a turn with that turn's id; once
	// the turn stops, the queued prompt runs without another UserPromptSubmit.
	// Its first call shows the session at work again.
	stopped := play(t, stopWith())
	is(t, stopped, adapter.Idle, "")
	start := startOf("", "Bash", "c9")
	start.Turn = "p9"
	rec, _ := Apply(stopped, start, t0)
	is(t, rec, adapter.Working, "")
	if rec.Turn != "p9" {
		t.Fatalf("turn %q", rec.Turn)
	}
}

func TestLateIdleNotification(t *testing.T) {
	idle := adapter.Transition{Action: adapter.Set, State: adapter.Idle, Turn: "p1", Late: true}
	// About this turn: it ended without Stop.
	is(t, play(t, idle), adapter.Idle, "")
	// About the turn before: the session has moved on.
	rec := play(t, submit, idle)
	is(t, rec, adapter.Working, "")
	// Without a turn it cannot be placed: it changes nothing.
	idle.Turn = ""
	is(t, play(t, idle), adapter.Working, "")
	// A turnless Set that is not late keeps the turn it cannot replace.
	turnless := adapter.Transition{Action: adapter.Set, State: adapter.Idle}
	if rec = play(t, turnless); rec.State() != adapter.Idle || rec.Turn != "p1" {
		t.Fatalf("turnless Set left %+v", rec)
	}
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
	ctx := context.Background()
	s := Store{Dir: t.TempDir()}
	rec := play(t, askFor("", "Bash"))
	if err := s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, proc.Claude, "s1")
	if err != nil || !ok || !got.equal(rec) {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	all, faults, err := s.All(ctx)
	if err != nil || len(all) != 1 || len(faults) != 0 {
		t.Fatalf("All = %+v, %v, %v", all, faults, err)
	}
	if err := s.Delete(ctx, proc.Claude, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, proc.Claude, "s1"); ok {
		t.Fatal("deleted session still there")
	}
	for _, id := range []string{"", "../x", "a/b", ".hidden"} {
		if err := s.Put(ctx, Record{Agent: proc.Claude, ID: id}); err == nil {
			t.Errorf("Put accepted id %q", id)
		}
	}
	if err := s.Put(ctx, Record{Agent: "aider", ID: "s1"}); err == nil {
		t.Error("Put accepted an unknown agent")
	}
}

func TestEventsReportTheWholeSession(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	ctx := context.Background()
	last := func() eventlog.Event {
		all, _, err := log.Since(ctx, 0)
		if err != nil || len(all) == 0 {
			t.Fatalf("events: %v %v", all, err)
		}
		return all[len(all)-1]
	}

	rec, _ := Apply(Record{Agent: proc.Codex, ID: "s1", Cwd: "/a", Host: Host{PID: 9, Role: proc.Service}, Transcript: "/t.jsonl"},
		adapter.Transition{Action: adapter.Set, State: adapter.Idle}, t0)
	if err := s.Save(ctx, log, Record{}, rec, eventlog.Event{Event: "SessionStart"}); err != nil {
		t.Fatal(err)
	}
	ev := last()
	if ev.Cwd != "/a" || ev.Runtime != "service" || ev.Transcript == nil ||
		*ev.Transcript != (eventlog.Transcript{Path: "/t.jsonl", Format: "codex-rollout"}) || ev.Subagents != nil {
		t.Fatalf("first event = %+v", ev)
	}

	// Each of these changes nothing but what an event reports, and is logged.
	moved := rec
	moved.Cwd = "/b"
	if err := s.Save(ctx, log, rec, moved, eventlog.Event{Event: "PreToolUse"}); err != nil || last().Event != "PreToolUse" || last().Cwd != "/b" {
		t.Fatalf("cwd change: %v %+v", err, last())
	}
	spawned, _ := Apply(moved, spawnOf("a1"), t0)
	if err := s.Save(ctx, log, moved, spawned, eventlog.Event{Event: "SubagentStart"}); err != nil || strings.Join(last().Subagents, ",") != "a1" {
		t.Fatalf("subagent start: %v %+v", err, last())
	}
	reaped, _ := Apply(spawned, reapOf("a1"), t0)
	if err := s.Save(ctx, log, spawned, reaped, eventlog.Event{Event: "SubagentStop"}); err != nil || last().Event != "SubagentStop" || last().Subagents != nil {
		t.Fatalf("subagent stop: %v %+v", err, last())
	}
	// A subagent's tool call changes the record but nothing an event reports.
	before := last().Seq
	called, _ := Apply(reaped, startOf("a2", "Bash", "c1"), t0)
	if err := s.Save(ctx, log, reaped, called, eventlog.Event{Event: "PreToolUse"}); err != nil || last().Seq != before {
		t.Fatalf("a call alone was logged: %v %+v", err, last())
	}
	if stored, _, _ := s.Get(ctx, proc.Codex, "s1"); len(stored.Calls) != 1 {
		t.Fatalf("the call was not stored: %+v", stored)
	}
}

func TestMainAgentReportClearsAnInterrupt(t *testing.T) {
	rec := play(t)
	rec.Halted = t0
	if sub, _ := Apply(rec, startOf("a1", "Read", "r1"), t0); sub.Halted.IsZero() {
		t.Fatal("a subagent's call cleared the interrupt")
	}
	if idle, _ := Apply(rec, stopWith(), t0); !idle.Halted.IsZero() {
		t.Fatal("the main agent's Stop left the interrupt standing")
	}
}

func TestMainAgentReportClearsQueued(t *testing.T) {
	rec := play(t)
	rec.Queued = t0
	if sub, _ := Apply(rec, startOf("a1", "Read", "r1"), t0); sub.Queued.IsZero() {
		t.Fatal("a subagent's call cleared the queued mark")
	}
	if idle, _ := Apply(rec, stopWith(), t0); !idle.Queued.IsZero() {
		t.Fatal("the main agent's Stop left the queued mark standing")
	}
	// Clearing it alone is a change to store even when the state reports the
	// same session.
	cur := play(t)
	cur.Queued = t0
	next, changed := Apply(cur, adapter.Transition{Action: adapter.Set, State: adapter.Working, Turn: "p1"}, t0)
	if !changed || !next.Queued.IsZero() {
		t.Fatalf("a report that only cleared queued changed %v: %+v", changed, next)
	}
}

// A request answered through tmag is tmag's synthetic transition, not the
// main agent reporting: the queued mark stands until the agent's next hook.
func TestSyntheticAnswerKeepsQueued(t *testing.T) {
	main := adapter.Prompt{Tool: "Bash"}
	sub := adapter.Prompt{Agent: "a1", Tool: "Bash"}
	rec := Record{
		Agent: proc.Claude, ID: "s1", Main: adapter.Working, Turn: "t1", Since: t0, Queued: t0,
		Pending:  []adapter.Prompt{main, sub},
		Requests: []Request{{ID: "r1", Prompt: main, Since: t0}, {ID: "r2", Prompt: sub, Since: t0}},
	}
	for _, id := range []string{"r1", "r2"} {
		got, _ := Apply(rec, adapter.Transition{Action: adapter.Answer, Request: id, Synthetic: true}, t0)
		if got.Queued.IsZero() {
			t.Errorf("answering request %s cleared queued", id)
		}
	}
	if got, _ := Apply(rec, adapter.Transition{Action: adapter.Set, State: adapter.Idle, Turn: "t1"}, t0); !got.Queued.IsZero() {
		t.Fatal("a main-agent report left queued standing")
	}
}

func TestRecordComparesTheInbox(t *testing.T) {
	rec := play(t)
	with := rec
	with.Inbox = &Inbox{Path: "/tmp/claude.sock", PID: 10, Start: "s10"}
	if with.equal(rec) {
		t.Fatal("an inbox does not make a record different")
	}
	same := with
	same.Inbox = &Inbox{Path: "/tmp/claude.sock", PID: 10, Start: "s10"}
	if !same.equal(with) {
		t.Fatal("equal inboxes make records different")
	}
}

func TestRequestsNameOpenPrompts(t *testing.T) {
	ask := func(tool, id string, waits bool) adapter.Transition {
		tr := askFor("", tool)
		tr.Request, tr.Waits = id, waits
		return tr
	}
	ids := func(rec Record) string {
		var out []string
		for _, q := range rec.Requests {
			out = append(out, q.ID)
		}
		return strings.Join(out, ",")
	}

	// A plain hook names a new prompt, not one already open.
	rec := play(t, ask("Bash", "r1", false), ask("Bash", "r2", false))
	if ids(rec) != "r1" {
		t.Fatalf("requests = %s", ids(rec))
	}
	// An answer hook takes over the name no hook waits on, then adds its own.
	rec, _ = Apply(rec, ask("Bash", "h1", true), t0)
	rec, _ = Apply(rec, ask("Bash", "h2", true), t0)
	if ids(rec) != "h1,h2" || !rec.Requests[0].Hook {
		t.Fatalf("requests = %+v", rec.Requests)
	}
	// They close with their prompt.
	rec, _ = Apply(rec, finishOf("", "Bash", "b1"), t0)
	if len(rec.Requests) != 0 || len(rec.Pending) != 0 {
		t.Fatalf("after the call: %+v %+v", rec.Pending, rec.Requests)
	}

	plan := play(t, ask("ExitPlanMode", "p1", true), ask("AskUserQuestion", "q1", true))
	refs := plan.RequestRefs()
	if len(refs) != 2 || refs[0].Kind != "plan" || refs[1].Kind != "question" || refs[1].Tool != "AskUserQuestion" {
		t.Fatalf("refs = %+v", refs)
	}
	if plan.Reports().Requests != "p1\nq1" {
		t.Fatalf("report = %+v", plan.Reports())
	}
	var ev eventlog.Event
	plan.Report(&ev)
	if len(ev.Requests) != 2 || ev.Requests[0].ID != "p1" {
		t.Fatalf("event requests = %+v", ev.Requests)
	}
	if id := NewRequestID(); len(id) != 12 || id == NewRequestID() {
		t.Fatalf("request id %q", id)
	}
}

func TestRequestIsAboutTheOneCallItCanBe(t *testing.T) {
	ask := func(agent, tool, id string) adapter.Transition {
		tr := askFor(agent, tool)
		tr.Request = id
		return tr
	}
	call := func(rec Record) string { return rec.Requests[0].Call }

	// One running call of the tool: the request is about it.
	rec := play(t, startOf("", "Bash", "a"), ask("", "Bash", "r1"))
	if call(rec) != "a" {
		t.Fatalf("request = %+v", rec.Requests)
	}
	// Approved in the terminal, a still runs; b asks next. Either may be
	// asking now, so the request is about neither.
	rec = play(t, startOf("", "Bash", "a"), ask("", "Bash", "r1"), startOf("", "Bash", "b"), ask("", "Bash", "r1"))
	if len(rec.Requests) != 1 || call(rec) != "" {
		t.Fatalf("request = %+v", rec.Requests)
	}
	// a finished, b asks: the request is about b from now on.
	rec = play(t, startOf("", "Bash", "a"), ask("", "Bash", "r1"), startOf("", "Bash", "b"), finishOf("", "Bash", "a"), ask("", "Bash", "r1"))
	if call(rec) != "b" {
		t.Fatalf("request = %+v", rec.Requests)
	}
	// Another tool's call, or another agent's, is no candidate.
	rec = play(t, startOf("", "Bash", "a"), startOf("", "write_stdin", "w"), startOf("sub", "Bash", "s"), ask("", "Bash", "r1"))
	if call(rec) != "a" {
		t.Fatalf("request = %+v", rec.Requests)
	}
	// An Ask that names its call is about that call.
	tr := ask("", "AskUserQuestion", "q1")
	tr.Call = adapter.Call{Tool: "AskUserQuestion", ID: "q"}
	if rec = play(t, startOf("", "AskUserQuestion", "x"), tr); call(rec) != "q" {
		t.Fatalf("request = %+v", rec.Requests)
	}
	// No running call: about none known.
	if rec = play(t, ask("", "Bash", "r1")); call(rec) != "" {
		t.Fatalf("request = %+v", rec.Requests)
	}
}

func TestAnsweringARequestClosesItsPromptOnce(t *testing.T) {
	ask := func(id string) adapter.Transition {
		tr := askFor("", "Bash")
		tr.Request, tr.Waits = id, true
		return tr
	}
	answered := func(id string) adapter.Transition { return adapter.Transition{Action: adapter.Answer, Request: id} }
	// Two dialogs for equal prompts: the first answer leaves the other open.
	rec := play(t, ask("h1"), ask("h2"))
	rec, _ = Apply(rec, answered("h1"), t0)
	if len(rec.Requests) != 1 || rec.Requests[0].ID != "h2" {
		t.Fatalf("after one answer: %+v", rec.Requests)
	}
	is(t, rec, adapter.Blocked, adapter.KindPermission)
	rec, _ = Apply(rec, answered("h2"), t0)
	if len(rec.Requests) != 0 || len(rec.Pending) != 0 {
		t.Fatalf("after both answers: %+v %+v", rec.Pending, rec.Requests)
	}
	is(t, rec, adapter.Working, "")
	if _, changed := Apply(rec, answered("h2"), t0); changed {
		t.Fatal("answering a closed request changed the session")
	}
}
