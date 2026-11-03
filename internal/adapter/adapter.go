// Package adapter maps agent hook payloads onto tmag state transitions.
//
// Payload deliberately declares no prompt, message or tool-input fields: content
// is discarded at decode time and can never reach tmux options or the event log.
package adapter

import (
	"encoding/json"
	"fmt"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Agent states.
const (
	Working = "working"
	Blocked = "blocked"
	Idle    = "idle"
	Unknown = "unknown"
)

// Kinds qualify a state.
const (
	KindPermission = "permission" // blocked on a tool permission prompt
	KindQuestion   = "question"   // blocked on a question to the user
	KindBackground = "background" // working on background tasks after the turn ended
	KindError      = "error"      // idle after a failed turn
)

// Payload is the subset of hook input tmag reads. Claude Code and Codex share
// these field names.
type Payload struct {
	HookEventName    string          `json:"hook_event_name"`
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	Source           string          `json:"source"`
	PromptID         string          `json:"prompt_id"`
	TurnID           string          `json:"turn_id"`
	AgentID          string          `json:"agent_id"`
	ToolName         string          `json:"tool_name"`
	NotificationType string          `json:"notification_type"`
	BackgroundTasks  json.RawMessage `json:"background_tasks"`
}

// Decode parses a hook payload.
func Decode(data []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("decode hook payload: %w", err)
	}
	if p.SessionID == "" || p.HookEventName == "" {
		return p, fmt.Errorf("hook payload lacks session_id or hook_event_name")
	}
	return p, nil
}

// Action says what a transition does to the agent record.
type Action int

const (
	Ignore Action = iota // event carries no state information
	Set                  // set state, kind and turn; every open prompt is over
	Ask                  // a prompt for the user opened; the agent is blocked
	Answer               // a prompt was answered; the agent works on once none is open
	End                  // the session ended; drop the record
)

// Prompt is something an agent waits on the user for: a tool permission, a
// question or an MCP elicitation, raised by the main agent or a subagent.
type Prompt struct {
	// Agent is the subagent that asked, "" for the main agent.
	Agent string
	// Tool is the tool asked about, or "" when the event does not say; any
	// completed tool answers an unnamed prompt.
	Tool string
}

// Pseudo tool names for prompts that are not tool calls.
const elicitation = "@elicitation"

// askUserQuestion is Claude Code's tool for asking the user a question.
const askUserQuestion = "AskUserQuestion"

// Kind is the record kind a prompt blocks with.
func (p Prompt) Kind() string {
	if p.Tool == askUserQuestion || p.Tool == elicitation {
		return KindQuestion
	}
	return KindPermission
}

// Transition is the effect of one hook event.
type Transition struct {
	Action Action
	State  string
	Kind   string
	Turn   string
	// Prompt is the prompt an Ask opens or an Answer closes.
	Prompt Prompt
	// Reminder marks an Ask that may repeat a prompt already open: it opens
	// one only during a turn with no prompt pending.
	Reminder bool
}

// Map returns the transition for a payload from the given agent.
func Map(agent string, p Payload) Transition {
	switch agent {
	case proc.Claude:
		return mapClaude(p)
	case proc.Codex:
		return mapCodex(p)
	}
	return Transition{}
}

func turn(p Payload) string {
	if p.PromptID != "" {
		return p.PromptID
	}
	return p.TurnID
}

// Subagent turns run inside the parent session; only their prompts for the user
// (permissions, questions) change what the session needs.
func subagentLifecycle(p Payload) bool {
	if p.AgentID == "" {
		return false
	}
	switch p.HookEventName {
	case "SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "Interrupt":
		return true
	}
	return false
}

func ask(p Payload, tool string) Transition {
	pr := Prompt{Agent: p.AgentID, Tool: tool}
	return Transition{Action: Ask, State: Blocked, Kind: pr.Kind(), Turn: turn(p), Prompt: pr}
}

func answer(p Payload, tool string) Transition {
	return Transition{Action: Answer, Turn: turn(p), Prompt: Prompt{Agent: p.AgentID, Tool: tool}}
}

func mapClaude(p Payload) Transition {
	if subagentLifecycle(p) {
		return Transition{}
	}
	switch p.HookEventName {
	case "SessionStart":
		if p.Source == "compact" {
			return Transition{}
		}
		return Transition{Action: Set, State: Idle}
	case "UserPromptSubmit":
		return Transition{Action: Set, State: Working, Turn: turn(p)}
	case "PreToolUse":
		if p.ToolName == askUserQuestion {
			return ask(p, askUserQuestion)
		}
	case "PermissionRequest":
		// AskUserQuestion also passes through the permission flow; it is
		// still a question for the user, not a tool approval.
		return ask(p, p.ToolName)
	case "Elicitation":
		return ask(p, elicitation)
	case "PostToolUse", "PostToolUseFailure":
		return answer(p, p.ToolName)
	case "ElicitationResult":
		return answer(p, elicitation)
	case "Notification":
		switch p.NotificationType {
		case "idle_prompt":
			// Safety net for turns that ended without Stop, e.g. user interrupts.
			return Transition{Action: Set, State: Idle}
		case "permission_prompt":
			// A prompt left open for a few seconds. It is the only signal for a
			// sandboxed command's network request, which raises no
			// PermissionRequest; for a tool it repeats one already pending.
			tr := ask(p, "")
			tr.Reminder = true
			return tr
		}
	case "Stop":
		if hasBackgroundTasks(p.BackgroundTasks) {
			return Transition{Action: Set, State: Working, Kind: KindBackground, Turn: turn(p)}
		}
		return Transition{Action: Set, State: Idle, Turn: turn(p)}
	case "StopFailure":
		return Transition{Action: Set, State: Idle, Kind: KindError, Turn: turn(p)}
	case "SessionEnd":
		return Transition{Action: End}
	}
	return Transition{}
}

func mapCodex(p Payload) Transition {
	if subagentLifecycle(p) {
		return Transition{}
	}
	switch p.HookEventName {
	case "SessionStart":
		if p.Source == "compact" {
			return Transition{}
		}
		return Transition{Action: Set, State: Idle}
	case "UserPromptSubmit":
		return Transition{Action: Set, State: Working, Turn: turn(p)}
	case "PermissionRequest":
		return ask(p, p.ToolName)
	case "PostToolUse":
		return answer(p, p.ToolName)
	case "Stop", "Interrupt":
		return Transition{Action: Set, State: Idle, Turn: turn(p)}
	case "SessionEnd":
		return Transition{Action: End}
	}
	return Transition{}
}

func hasBackgroundTasks(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tasks []json.RawMessage
	if json.Unmarshal(raw, &tasks) == nil {
		return len(tasks) > 0
	}
	return false
}
