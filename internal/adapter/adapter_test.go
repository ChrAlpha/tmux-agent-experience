package adapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func payload(t *testing.T, fields map[string]any) Payload {
	t.Helper()
	if _, ok := fields["session_id"]; !ok {
		fields["session_id"] = "s1"
	}
	b, _ := json.Marshal(fields)
	p, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMap(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		in    map[string]any
		want  Transition
	}{
		{"claude start", "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup"}, Transition{Action: Set, State: Idle, Fresh: true}},
		{"claude compact keeps state", "claude", map[string]any{"hook_event_name": "SessionStart", "source": "compact"}, Transition{}},
		{"claude prompt", "claude", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1", "prompt": "secret"}, Transition{Action: Set, State: Working, Turn: "p1", KeepSubagents: true}},
		{"claude question", "claude", map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "q1", "prompt_id": "p1"}, Transition{Action: Ask, State: Blocked, Kind: KindQuestion, Turn: "p1", Prompt: Prompt{Tool: "AskUserQuestion"}, Call: Call{Tool: "AskUserQuestion", ID: "q1"}}},
		{"claude tool start", "claude", map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "u1", "agent_id": "a1"}, Transition{Action: Start, Call: Call{Agent: "a1", Tool: "Bash", ID: "u1"}}},
		{"claude permission", "claude", map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash"}, Transition{Action: Ask, State: Blocked, Kind: KindPermission, Prompt: Prompt{Tool: "Bash"}}},
		{"claude question via permission flow", "claude", map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "AskUserQuestion"}, Transition{Action: Ask, State: Blocked, Kind: KindQuestion, Prompt: Prompt{Tool: "AskUserQuestion"}}},
		{"claude subagent permission", "claude", map[string]any{"hook_event_name": "PermissionRequest", "agent_id": "a1", "tool_name": "Bash"}, Transition{Action: Ask, State: Blocked, Kind: KindPermission, Prompt: Prompt{Agent: "a1", Tool: "Bash"}}},
		{"claude elicitation", "claude", map[string]any{"hook_event_name": "Elicitation"}, Transition{Action: Ask, State: Blocked, Kind: KindQuestion, Prompt: Prompt{Tool: elicitation}}},
		{"claude elicitation result", "claude", map[string]any{"hook_event_name": "ElicitationResult"}, Transition{Action: Answer, Prompt: Prompt{Tool: elicitation}}},
		{"claude tool finish", "claude", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "u1", "agent_id": "a1"}, Transition{Action: Finish, Call: Call{Agent: "a1", Tool: "Bash", ID: "u1"}}},
		{"claude failed tool finish", "claude", map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "Bash", "tool_use_id": "u2"}, Transition{Action: Finish, Call: Call{Tool: "Bash", ID: "u2"}}},
		{"claude batch", "claude", map[string]any{"hook_event_name": "PostToolBatch", "agent_id": "a1", "tool_calls": []any{map[string]any{"tool_response": "secret"}}}, Transition{Action: Settle, Call: Call{Agent: "a1"}}},
		{"claude subagent start", "claude", map[string]any{"hook_event_name": "SubagentStart", "agent_id": "a1", "agent_type": "Explore"}, Transition{Action: Spawn, Call: Call{Agent: "a1"}}},
		{"claude subagent stop", "claude", map[string]any{"hook_event_name": "SubagentStop", "agent_id": "a1"}, Transition{Action: Reap, Call: Call{Agent: "a1"}}},
		{"claude internal agent stop without id ignored", "claude", map[string]any{"hook_event_name": "SubagentStop"}, Transition{}},
		{"claude idle notification", "claude", map[string]any{"hook_event_name": "Notification", "notification_type": "idle_prompt", "prompt_id": "p1"}, Transition{Action: Set, State: Idle, Turn: "p1", Late: true}},
		{"claude permission notification", "claude", map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt"}, Transition{Action: Ask, State: Blocked, Kind: KindPermission, Reminder: true}},
		{"claude other notification", "claude", map[string]any{"hook_event_name": "Notification", "notification_type": "auth_success"}, Transition{}},
		{"claude stop", "claude", map[string]any{"hook_event_name": "Stop", "prompt_id": "p1", "background_tasks": []any{}}, Transition{Action: Set, State: Idle, Turn: "p1", Background: &[]string{}}},
		{"claude stop without task list", "claude", map[string]any{"hook_event_name": "Stop", "prompt_id": "p1"}, Transition{Action: Set, State: Idle, Turn: "p1"}},
		{"claude stop with background", "claude", map[string]any{"hook_event_name": "Stop", "background_tasks": []any{map[string]any{"id": "a7", "type": "subagent", "description": "secret"}}}, Transition{Action: Set, State: Idle, Background: &[]string{"a7"}}},
		{"claude stop failure", "claude", map[string]any{"hook_event_name": "StopFailure"}, Transition{Action: Set, State: Idle, Kind: KindError, KeepSubagents: true}},
		{"claude subagent stop ignored", "claude", map[string]any{"hook_event_name": "Stop", "agent_id": "a1"}, Transition{}},
		{"claude end", "claude", map[string]any{"hook_event_name": "SessionEnd", "reason": "other"}, Transition{Action: End}},
		{"codex start", "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup"}, Transition{Action: Set, State: Idle, Fresh: true}},
		{"codex prompt", "codex", map[string]any{"hook_event_name": "UserPromptSubmit", "turn_id": "t1"}, Transition{Action: Set, State: Working, Turn: "t1", KeepSubagents: true}},
		{"codex subagent prompt ignored", "codex", map[string]any{"hook_event_name": "UserPromptSubmit", "turn_id": "t1", "agent_id": "a"}, Transition{}},
		{"codex permission", "codex", map[string]any{"hook_event_name": "PermissionRequest", "turn_id": "t1", "tool_name": "shell"}, Transition{Action: Ask, State: Blocked, Kind: KindPermission, Turn: "t1", Prompt: Prompt{Tool: "shell"}}},
		{"codex tool start", "codex", map[string]any{"hook_event_name": "PreToolUse", "turn_id": "t1", "tool_name": "Bash", "tool_use_id": "c1"}, Transition{Action: Start, Turn: "t1", Call: Call{Tool: "Bash", ID: "c1"}}},
		{"codex tool finish", "codex", map[string]any{"hook_event_name": "PostToolUse", "turn_id": "t1", "tool_name": "Bash", "tool_use_id": "c1"}, Transition{Action: Finish, Turn: "t1", Call: Call{Tool: "Bash", ID: "c1"}}},
		{"codex model request", "codex", map[string]any{"hook_event_name": "PreModelRequest", "turn_id": "t2", "agent_id": "a"}, Transition{Action: Settle, Turn: "t2", Call: Call{Agent: "a"}}},
		{"codex interrupt", "codex", map[string]any{"hook_event_name": "Interrupt", "turn_id": "t1"}, Transition{Action: Set, State: Idle, Turn: "t1"}},
		{"codex stop", "codex", map[string]any{"hook_event_name": "Stop", "turn_id": "t1"}, Transition{Action: Set, State: Idle, Turn: "t1"}},
		{"codex subagent start", "codex", map[string]any{"hook_event_name": "SubagentStart", "turn_id": "t9", "agent_id": "a", "agent_type": "default"}, Transition{Action: Spawn, Turn: "t9", Call: Call{Agent: "a"}}},
		{"codex subagent stop", "codex", map[string]any{"hook_event_name": "SubagentStop", "turn_id": "t9", "agent_id": "a"}, Transition{Action: Reap, Turn: "t9", Call: Call{Agent: "a"}}},
		{"unknown agent", "aider", map[string]any{"hook_event_name": "Stop"}, Transition{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Map(c.agent, payload(t, c.in)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDecodeDropsContent(t *testing.T) {
	p := payload(t, map[string]any{
		"hook_event_name": "UserPromptSubmit", "prompt": "do not keep me",
		"last_assistant_message": "nor me", "tool_input": map[string]any{"command": "rm -rf /"},
		"background_tasks": []any{map[string]any{"id": "a7", "description": "or this"}},
	})
	b, _ := json.Marshal(p)
	for _, secret := range []string{"do not keep me", "nor me", "rm -rf", "or this"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("decoded payload retains %q: %s", secret, b)
		}
	}
}

func TestDecodeRejectsIncomplete(t *testing.T) {
	for _, in := range []string{`{}`, `{"session_id":"x"}`, `{"hook_event_name":"Stop"}`, `not json`} {
		if _, err := Decode([]byte(in)); err == nil {
			t.Errorf("Decode(%s) succeeded", in)
		}
	}
}
