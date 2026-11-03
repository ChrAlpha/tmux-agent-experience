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

func answerOf(agent, tool string) adapter.Transition {
	return adapter.Transition{Action: adapter.Answer, Prompt: adapter.Prompt{Agent: agent, Tool: tool}}
}

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
	if !changed || rec.State != adapter.Blocked {
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
