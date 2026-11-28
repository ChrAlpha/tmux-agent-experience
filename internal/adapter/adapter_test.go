package adapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
		{"claude stop failure names why", "claude", map[string]any{"hook_event_name": "StopFailure", "error": "model_not_found"}, Transition{Action: Set, State: Idle, Kind: KindError, Reason: "model_not_found", KeepSubagents: true}},
		{"claude compacts", "claude", map[string]any{"hook_event_name": "PreCompact", "trigger": "auto", "prompt_id": "p1"}, Transition{Action: Set, State: Working, Kind: KindCompacting, Turn: "p1", KeepSubagents: true}},
		{"claude manual compaction", "claude", map[string]any{"hook_event_name": "PreCompact", "trigger": "manual", "prompt_id": "p1"}, Transition{Action: Set, State: Working, Kind: KindCompacting, Turn: "p1", KeepSubagents: true}},
		{"claude compacted automatically", "claude", map[string]any{"hook_event_name": "PostCompact", "trigger": "auto", "prompt_id": "p1"}, Transition{Action: Set, State: Working, Turn: "p1", KeepSubagents: true}},
		{"claude compacted by hand", "claude", map[string]any{"hook_event_name": "PostCompact", "trigger": "manual", "prompt_id": "p1"}, Transition{Action: Set, State: Idle, Turn: "p1", KeepSubagents: true}},
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
		{"codex compacts by hand", "codex", map[string]any{"hook_event_name": "PreCompact", "trigger": "manual", "turn_id": "t2"}, Transition{Action: Set, State: Working, Kind: KindCompacting, Turn: "t2", KeepSubagents: true}},
		{"codex compacted automatically", "codex", map[string]any{"hook_event_name": "PostCompact", "trigger": "auto", "turn_id": "t2"}, Transition{Action: Set, State: Working, Turn: "t2", KeepSubagents: true}},
		{"codex compacted by hand", "codex", map[string]any{"hook_event_name": "PostCompact", "trigger": "manual", "turn_id": "t2"}, Transition{Action: Set, State: Idle, Turn: "t2", KeepSubagents: true}},
		{"codex subagent start", "codex", map[string]any{"hook_event_name": "SubagentStart", "turn_id": "t9", "agent_id": "a", "agent_type": "default"}, Transition{Action: Spawn, Turn: "t9", Call: Call{Agent: "a"}}},
		{"codex subagent stop", "codex", map[string]any{"hook_event_name": "SubagentStop", "turn_id": "t9", "agent_id": "a"}, Transition{Action: Reap, Turn: "t9", Call: Call{Agent: "a"}}},
		{"pi start", "pi", map[string]any{"hook_event_name": "session_start", "source": "startup"}, Transition{Action: Set, State: Idle, Fresh: true}},
		{"pi resume names its last turn", "pi", map[string]any{"hook_event_name": "session_start", "source": "resume", "turn_id": "73544b94"}, Transition{Action: Set, State: Idle, Turn: "73544b94", Fresh: true}},
		{"pi reload keeps state", "pi", map[string]any{"hook_event_name": "session_start", "source": "reload"}, Transition{}},
		{"pi prompt", "pi", map[string]any{"hook_event_name": "message_end", "turn_id": "73544b94"}, Transition{Action: Set, State: Working, Turn: "73544b94", KeepSubagents: true}},
		{"pi run without a prompt", "pi", map[string]any{"hook_event_name": "agent_start"}, Transition{Action: Set, State: Working, KeepSubagents: true}},
		{"pi tool start", "pi", map[string]any{"hook_event_name": "tool_execution_start", "tool_name": "bash", "tool_use_id": "call_1"}, Transition{Action: Start, Call: Call{Tool: "bash", ID: "call_1"}}},
		{"pi tool finish", "pi", map[string]any{"hook_event_name": "tool_execution_end", "tool_name": "bash", "tool_use_id": "call_1"}, Transition{Action: Finish, Call: Call{Tool: "bash", ID: "call_1"}}},
		{"pi dialog", "pi", map[string]any{"hook_event_name": "ui_prompt_start"}, Transition{Action: Ask, State: Blocked, Kind: KindQuestion, Prompt: Prompt{Tool: dialog}}},
		{"pi dialog closed", "pi", map[string]any{"hook_event_name": "ui_prompt_end"}, Transition{Action: Answer, Prompt: Prompt{Tool: dialog}}},
		{"pi settled", "pi", map[string]any{"hook_event_name": "agent_settled", "stop_reason": "aborted"}, Transition{Action: Set, State: Idle}},
		{"pi failed turn", "pi", map[string]any{"hook_event_name": "agent_settled", "stop_reason": "error"}, Transition{Action: Set, State: Idle, Kind: KindError}},
		{"pi compacts", "pi", map[string]any{"hook_event_name": "session_before_compact", "trigger": "manual", "turn_id": "t1"}, Transition{Action: Set, State: Working, Kind: KindCompacting, Turn: "t1", KeepSubagents: true}},
		{"pi compacted automatically", "pi", map[string]any{"hook_event_name": "session_compact", "trigger": "auto", "turn_id": "t1"}, Transition{Action: Set, State: Working, Turn: "t1", KeepSubagents: true}},
		{"pi compacted by hand", "pi", map[string]any{"hook_event_name": "session_compact", "trigger": "manual", "turn_id": "t1"}, Transition{Action: Set, State: Idle, Turn: "t1", KeepSubagents: true}},
		{"pi compaction failed", "pi", map[string]any{"hook_event_name": "session_compact_failed", "trigger": "manual", "turn_id": "t1"}, Transition{Action: Set, State: Idle, Turn: "t1", KeepSubagents: true, OnlyCompacting: true}},
		{"pi end", "pi", map[string]any{"hook_event_name": "session_shutdown", "reason": "new"}, Transition{Action: End}},
		{"pi reload goes on", "pi", map[string]any{"hook_event_name": "session_shutdown", "reason": "reload"}, Transition{}},
		{"omp start", "omp", map[string]any{"hook_event_name": "session_start"}, Transition{Action: Set, State: Idle, Fresh: true}},
		{"omp switched to", "omp", map[string]any{"hook_event_name": "session_switch", "source": "resume", "turn_id": "ad08e5b4"}, Transition{Action: Set, State: Idle, Turn: "ad08e5b4", Fresh: true}},
		{"omp prompt", "omp", map[string]any{"hook_event_name": "message_end", "turn_id": "ad08e5b4"}, Transition{Action: Set, State: Working, Turn: "ad08e5b4", KeepSubagents: true}},
		{"omp tool start", "omp", map[string]any{"hook_event_name": "tool_execution_start", "tool_name": "bash", "tool_use_id": "c1"}, Transition{Action: Start, Call: Call{Tool: "bash", ID: "c1"}}},
		{"omp question", "omp", map[string]any{"hook_event_name": "tool_execution_start", "tool_name": "ask", "tool_use_id": "c2"}, Transition{Action: Ask, State: Blocked, Kind: KindQuestion, Prompt: Prompt{Tool: ompAsk}, Call: Call{Tool: ompAsk, ID: "c2"}}},
		{"omp question answered", "omp", map[string]any{"hook_event_name": "tool_execution_end", "tool_name": "ask", "tool_use_id": "c2"}, Transition{Action: Finish, Call: Call{Tool: ompAsk, ID: "c2"}}},
		{"omp approval", "omp", map[string]any{"hook_event_name": "tool_approval_requested", "tool_name": "bash", "tool_use_id": "c3"}, Transition{Action: Ask, State: Blocked, Kind: KindPermission, Prompt: Prompt{Tool: "bash"}, Call: Call{Tool: "bash", ID: "c3"}}},
		{"omp approval decided", "omp", map[string]any{"hook_event_name": "tool_approval_resolved", "tool_name": "bash", "tool_use_id": "c3"}, Transition{Action: Answer, Prompt: Prompt{Tool: "bash"}}},
		{"omp end of turn", "omp", map[string]any{"hook_event_name": "agent_end", "stop_reason": "stop"}, Transition{Action: Set, State: Idle}},
		{"omp failed turn", "omp", map[string]any{"hook_event_name": "agent_end", "stop_reason": "error"}, Transition{Action: Set, State: Idle, Kind: KindError}},
		{"omp end", "omp", map[string]any{"hook_event_name": "session_shutdown", "reason": "quit"}, Transition{Action: End}},
		{"declared start", "gemini", map[string]any{"hook_event_name": "SessionStart", "source": "startup"}, Transition{Action: Set, State: Idle, Fresh: true}},
		{"declared resume names its last turn", "gemini", map[string]any{"hook_event_name": "SessionStart", "source": "resume", "turn_id": "t4"}, Transition{Action: Set, State: Idle, Turn: "t4", Fresh: true}},
		{"declared prompt", "gemini", map[string]any{"hook_event_name": "UserPromptSubmit", "turn_id": "t1", "prompt": "secret"}, Transition{Action: Set, State: Working, Turn: "t1", KeepSubagents: true}},
		{"declared prompt named as claude does", "gemini", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": "p1"}, Transition{Action: Set, State: Working, Turn: "p1", KeepSubagents: true}},
		{"declared tool start", "gemini", map[string]any{"hook_event_name": "PreToolUse", "turn_id": "t1", "tool_name": "AskUserQuestion", "tool_use_id": "c1"}, Transition{Action: Start, Turn: "t1", Call: Call{Tool: "AskUserQuestion", ID: "c1"}}},
		{"declared permission", "gemini", map[string]any{"hook_event_name": "PermissionRequest", "turn_id": "t1", "tool_name": "ExitPlanMode"}, Transition{Action: Ask, State: Blocked, Kind: KindPermission, Turn: "t1", Prompt: Prompt{Tool: "ExitPlanMode"}}},
		{"declared question", "gemini", map[string]any{"hook_event_name": "Elicitation", "turn_id": "t1"}, Transition{Action: Ask, State: Blocked, Kind: KindQuestion, Turn: "t1", Prompt: Prompt{Tool: elicitation}}},
		{"declared question answered", "gemini", map[string]any{"hook_event_name": "ElicitationResult", "turn_id": "t1"}, Transition{Action: Answer, Turn: "t1", Prompt: Prompt{Tool: elicitation}}},
		{"declared tool finish", "gemini", map[string]any{"hook_event_name": "PostToolUse", "turn_id": "t1", "tool_name": "shell", "tool_use_id": "c1"}, Transition{Action: Finish, Turn: "t1", Call: Call{Tool: "shell", ID: "c1"}}},
		{"declared failed tool finish", "gemini", map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "shell", "tool_use_id": "c2"}, Transition{Action: Finish, Call: Call{Tool: "shell", ID: "c2"}}},
		{"declared batch", "gemini", map[string]any{"hook_event_name": "PostToolBatch", "turn_id": "t1"}, Transition{Action: Settle, Turn: "t1"}},
		{"declared model request", "gemini", map[string]any{"hook_event_name": "PreModelRequest", "turn_id": "t1", "agent_id": "a"}, Transition{Action: Settle, Turn: "t1", Call: Call{Agent: "a"}}},
		{"declared subagent start", "gemini", map[string]any{"hook_event_name": "SubagentStart", "agent_id": "a"}, Transition{Action: Spawn, Call: Call{Agent: "a"}}},
		{"declared subagent stop", "gemini", map[string]any{"hook_event_name": "SubagentStop", "agent_id": "a"}, Transition{Action: Reap, Call: Call{Agent: "a"}}},
		{"declared subagent prompt ignored", "gemini", map[string]any{"hook_event_name": "UserPromptSubmit", "turn_id": "t1", "agent_id": "a"}, Transition{}},
		{"declared stop", "gemini", map[string]any{"hook_event_name": "Stop", "turn_id": "t1", "background_tasks": []any{map[string]any{"id": "a7"}}}, Transition{Action: Set, State: Idle, Turn: "t1"}},
		{"declared interrupt", "gemini", map[string]any{"hook_event_name": "Interrupt", "turn_id": "t1"}, Transition{Action: Set, State: Idle, Turn: "t1"}},
		{"declared stop failure", "gemini", map[string]any{"hook_event_name": "StopFailure", "turn_id": "t1", "error": "rate_limit"}, Transition{Action: Set, State: Idle, Kind: KindError, Turn: "t1", Reason: "rate_limit", KeepSubagents: true}},
		{"declared compacts", "gemini", map[string]any{"hook_event_name": "PreCompact", "trigger": "auto", "turn_id": "t1"}, Transition{Action: Set, State: Working, Kind: KindCompacting, Turn: "t1", KeepSubagents: true}},
		{"declared compacted by hand", "gemini", map[string]any{"hook_event_name": "PostCompact", "trigger": "manual", "turn_id": "t1"}, Transition{Action: Set, State: Idle, Turn: "t1", KeepSubagents: true}},
		{"declared end", "gemini", map[string]any{"hook_event_name": "SessionEnd"}, Transition{Action: End}},
		{"declared notification ignored", "gemini", map[string]any{"hook_event_name": "Notification", "notification_type": "idle_prompt"}, Transition{}},
		{"declared names its own events otherwise", "gemini", map[string]any{"hook_event_name": "AfterAgent"}, Transition{}},
		{"undeclarable agent", "bash", map[string]any{"hook_event_name": "Stop"}, Transition{}},
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

// A payload's permission_mode keeps its class and nothing else: the raw mode
// never reaches a record, and an unknown one is dropped.
func TestDecodeKeepsPermissionClass(t *testing.T) {
	for mode, want := range map[string]string{
		"bypassPermissions": PermissionBypass,
		"default":           PermissionPrompting,
		"acceptEdits":       PermissionPrompting,
		"plan":              PermissionPrompting,
		"dontAsk":           PermissionPrompting,
		"":                  "",
		"allow":             "",
		"bypass\n":          "",
	} {
		p := payload(t, map[string]any{"hook_event_name": "PreToolUse", "permission_mode": mode})
		if p.PermissionClass() != want {
			t.Errorf("permission_mode %q is class %q, want %q", mode, p.PermissionClass(), want)
		}
		if want == "" && p.PermissionMode != "" {
			t.Errorf("permission_mode %q kept %q", mode, p.PermissionMode)
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

// What a payload keeps is bounded whatever an agent sends: a path longer
// than any path is dropped, names and ids are cut at a character boundary
// the same way every time, and a Stop lists a bounded number of tasks.
func TestDecodeBoundsWhatItKeeps(t *testing.T) {
	long := strings.Repeat("é", MaxName) // two bytes each
	tasks := make([]string, 0, 3*MaxTasks)
	for range 3 * MaxTasks {
		tasks = append(tasks, `{"id":"`+long+`"}`)
	}
	payload := `{"hook_event_name":"Stop","session_id":"s1","cwd":"/` + strings.Repeat("d", MaxPath) + `",` +
		`"transcript_path":"/t.jsonl","inbox":"/` + strings.Repeat("i", MaxPath) + `",` +
		`"tool_name":"` + long + `","prompt_id":"` + long + `",` +
		`"reason":"` + long + `","stop_reason":"` + long + `","mode":"` + long + `","error":"` + long + `","trigger":"` + long + `",` +
		`"background_tasks":[` + strings.Join(tasks, ",") + `]}`
	p, err := Decode([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if p.Cwd != "" || p.TranscriptPath != "/t.jsonl" || p.Inbox != "" {
		t.Errorf("cwd %d bytes, transcript %q, inbox %d bytes", len(p.Cwd), p.TranscriptPath, len(p.Inbox))
	}
	inboxed, err := Decode([]byte(`{"hook_event_name":"Stop","session_id":"s1","inbox":"/tmp/pi.sock"}`))
	if err != nil || inboxed.Inbox != "/tmp/pi.sock" {
		t.Errorf("inbox %q: %v", inboxed.Inbox, err)
	}
	for _, f := range []string{p.ToolName, p.PromptID, p.Reason, p.StopReason, p.Mode, p.Error, p.Trigger, (*p.BackgroundTasks)[0].ID} {
		if len(f) > MaxName || !utf8.ValidString(f) || !strings.HasPrefix(long, f) {
			t.Errorf("a name was not cut cleanly: %d bytes", len(f))
		}
	}
	if len(*p.BackgroundTasks) != MaxTasks {
		t.Errorf("%d background tasks kept", len(*p.BackgroundTasks))
	}
	again, _ := Decode([]byte(payload))
	if again.ToolName != p.ToolName {
		t.Error("the same name is cut differently")
	}
}
