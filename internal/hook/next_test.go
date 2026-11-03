package hook

import (
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/registry"
)

var id = registry.Record{Agent: "claude", Session: "s1", PID: 10, Binding: "process", Since: time.Unix(100, 0).UTC()}

// play applies events in order, starting from a working turn.
func play(t *testing.T, trs ...adapter.Transition) registry.Record {
	t.Helper()
	cur := id
	cur.State, cur.Turn = adapter.Working, "p1"
	for _, tr := range trs {
		if rec, changed := next(cur, tr, id); changed {
			cur = rec
		}
	}
	return cur
}

func askFor(agent, tool string) adapter.Transition {
	p := adapter.Prompt{Agent: agent, Tool: tool}
	return adapter.Transition{Action: adapter.Ask, State: adapter.Blocked, Kind: p.Kind(), Prompt: p}
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

// answerOf is a call of tool by agent that ran and finished.
func answerOf(agent, tool string) adapter.Transition { return finishOf(agent, tool, "x-"+agent+tool) }

func TestUnrelatedCompletionKeepsPromptOpen(t *testing.T) {
	// Subagent B waits for Bash approval while subagent A finishes its own Bash.
	rec := play(t, askFor("B", "Bash"), answerOf("A", "Bash"))
	if rec.State != adapter.Blocked || rec.Turn != "p1" {
		t.Fatalf("another agent's tool released the prompt: %+v", rec)
	}
	rec = play(t, askFor("B", "Bash"), answerOf("B", "Read"))
	if rec.State != adapter.Blocked {
		t.Fatalf("another tool released the prompt: %+v", rec)
	}
	rec = play(t, askFor("B", "Bash"), answerOf("B", "Bash"))
	if rec.State != adapter.Working || len(rec.Pending) != 0 {
		t.Fatalf("the prompt's own tool did not release it: %+v", rec)
	}
}

func TestQueuedPromptsBlockUntilAllAreAnswered(t *testing.T) {
	rec := play(t, askFor("A", "Bash"), askFor("", "AskUserQuestion"), answerOf("A", "Bash"))
	if rec.State != adapter.Blocked || rec.Kind != adapter.KindQuestion {
		t.Fatalf("answering one prompt released the other: %+v", rec)
	}
	rec = play(t, askFor("A", "Bash"), askFor("", "AskUserQuestion"), answerOf("A", "Bash"), answerOf("", "AskUserQuestion"))
	if rec.State != adapter.Working || rec.Kind != "" {
		t.Fatalf("all prompts answered, still %+v", rec)
	}
}

func TestUnnamedPrompt(t *testing.T) {
	// A sandboxed command's network request: only a notification, no
	// PermissionRequest. The command's completion answers it.
	rec := play(t, reminder())
	if rec.State != adapter.Blocked || rec.Kind != adapter.KindPermission {
		t.Fatalf("network approval not blocked: %+v", rec)
	}
	if rec = play(t, reminder(), answerOf("", "Bash")); rec.State != adapter.Working {
		t.Fatalf("completion did not answer it: %+v", rec)
	}
	// The notification may not name the subagent whose command asked.
	if rec = play(t, reminder(), answerOf("X", "Bash")); rec.State != adapter.Working {
		t.Fatalf("a subagent's completion did not answer it: %+v", rec)
	}
	// The notification repeating a pending PermissionRequest adds nothing.
	rec = play(t, askFor("A", "Bash"), reminder(), answerOf("A", "Bash"))
	if rec.State != adapter.Working {
		t.Fatalf("duplicate notification kept the agent blocked: %+v", rec)
	}
	// Outside a turn it is noise.
	idle := id
	idle.State = adapter.Idle
	if _, changed := next(idle, reminder(), id); changed {
		t.Fatal("notification blocked an idle agent")
	}
}

func TestUnnamedPermissionRequestBlocksAnywhere(t *testing.T) {
	// A permission request without a tool name is still a prompt, even as the
	// first event a pane sees; the next completion answers it.
	rec, changed := next(registry.Record{}, askFor("", ""), id)
	if !changed || rec.State != adapter.Blocked || rec.Base != "" {
		t.Fatalf("unnamed request = %+v, %v", rec, changed)
	}
	if rec, _ = next(rec, answerOf("", "shell"), id); rec.State != adapter.Working {
		t.Fatalf("completion left %+v", rec)
	}
}

func TestTurnEventsCloseEveryPrompt(t *testing.T) {
	stop := adapter.Transition{Action: adapter.Set, State: adapter.Idle, Turn: "p1"}
	rec := play(t, askFor("A", "Bash"), askFor("B", "Bash"), stop)
	if rec.State != adapter.Idle || len(rec.Pending) != 0 {
		t.Fatalf("Stop left %+v", rec)
	}
}

func TestIdleKeepsSubmittedPrompt(t *testing.T) {
	cur := id
	cur.State, cur.Sent = adapter.Idle, time.Unix(200, 0).UTC()
	notification := adapter.Transition{Action: adapter.Set, State: adapter.Idle}
	if _, changed := next(cur, notification, id); changed {
		t.Fatal("idle notification changed a pending record")
	}
	start := adapter.Transition{Action: adapter.Set, State: adapter.Working, Turn: "p2"}
	if rec, _ := next(cur, start, id); !rec.Sent.IsZero() {
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
		rec := play(t, order...)
		if rec.State != adapter.Blocked || len(rec.Pending) != 1 {
			t.Errorf("%s: the allowed call released the prompt: %+v", name, rec)
		}
		rec = play(t, append(order, finishOf("", "Read", "r2"), finishOf("", "Read", "r1"))...)
		if rec.State != adapter.Working || rec.Pending != nil || rec.Calls != nil {
			t.Errorf("%s: both calls over, still %+v", name, rec)
		}
	}
}

func TestBatchSettlesDeniedCalls(t *testing.T) {
	// A call denied with Esc reports no PostToolUse; its batch still resolves.
	rec := play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), settleOf("w"))
	if rec.State != adapter.Working || rec.Pending != nil || rec.Calls != nil {
		t.Fatalf("batch left %+v", rec)
	}
	// Another agent's batch says nothing about it.
	rec = play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), settleOf(""))
	if rec.State != adapter.Blocked || len(rec.Calls) != 1 {
		t.Fatalf("main agent's batch released a worker's prompt: %+v", rec)
	}
}

func TestBackgroundWorkerPromptOutlivesTheTurn(t *testing.T) {
	// Claude 2.1.317: a background worker asks, then the main agent stops
	// with the worker still listed as running.
	stop := adapter.Transition{Action: adapter.Set, State: adapter.Working, Kind: adapter.KindBackground,
		Keep: adapter.Keep{Running: []string{"w"}}}
	rec := play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), askFor("", "Read"), stop)
	if rec.State != adapter.Blocked || rec.Kind != adapter.KindPermission ||
		len(rec.Pending) != 1 || rec.Pending[0].Agent != "w" || rec.Base != adapter.KindBackground {
		t.Fatalf("Stop hid the worker's prompt: %+v", rec)
	}
	// Answered: back to background work, not to a turn.
	done := play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), stop, finishOf("w", "Bash", "b1"))
	if done.State != adapter.Working || done.Kind != adapter.KindBackground || done.Base != "" {
		t.Fatalf("answered worker prompt left %+v", done)
	}
	// A new turn of the main agent, say a task notification, keeps it too.
	submit := adapter.Transition{Action: adapter.Set, State: adapter.Working, Turn: "p2", Keep: adapter.Keep{Subagents: true}}
	rec = play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), stop, submit)
	if rec.State != adapter.Blocked || rec.Base != "" || rec.Turn != "p2" {
		t.Fatalf("turn start hid the worker's prompt: %+v", rec)
	}
	// A Stop that no longer lists the worker ends it.
	final := adapter.Transition{Action: adapter.Set, State: adapter.Idle}
	rec = play(t, startOf("w", "Bash", "b1"), askFor("w", "Bash"), stop, final)
	if rec.State != adapter.Idle || rec.Pending != nil || rec.Calls != nil {
		t.Fatalf("final Stop left %+v", rec)
	}
}

func TestCallBookkeepingIsNotReported(t *testing.T) {
	cur := id
	cur.State, cur.Turn = adapter.Working, "p1"
	rec, changed := next(cur, startOf("", "Read", "r1"), id)
	if !changed || len(rec.Calls) != 1 || !reported(rec).Equal(reported(cur)) {
		t.Fatalf("start = %+v, %v", rec, changed)
	}
	if _, changed := next(registry.Record{}, startOf("", "Read", "r1"), id); changed {
		t.Fatal("a call created a record")
	}
}
