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
	Set                  // set state, kind and turn
	End                  // the session ended; drop the record
)

// Transition is the effect of one hook event.
type Transition struct {
	Action Action
	State  string
	Kind   string
	Turn   string
	// OnlyFrom restricts the transition to records currently in this state.
	OnlyFrom string
	// Tool is the tool a permission prompt is for, or, with OnlyFrom, the tool
	// whose completion releases the block. Empty matches any tool.
	Tool string
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

// askUserQuestion is Claude Code's tool for asking the user a question.
const askUserQuestion = "AskUserQuestion"

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
			return Transition{Action: Set, State: Blocked, Kind: KindQuestion, Turn: turn(p)}
		}
	case "PermissionRequest":
		// AskUserQuestion also passes through the permission flow; it is
		// still a question for the user, not a tool approval.
		kind := KindPermission
		if p.ToolName == askUserQuestion {
			kind = KindQuestion
		}
		return Transition{Action: Set, State: Blocked, Kind: kind, Turn: turn(p), Tool: p.ToolName}
	case "Elicitation":
		return Transition{Action: Set, State: Blocked, Kind: KindQuestion, Turn: turn(p)}
	case "PostToolUse", "PostToolUseFailure":
		return Transition{Action: Set, State: Working, Turn: turn(p), OnlyFrom: Blocked, Tool: p.ToolName}
	case "ElicitationResult":
		return Transition{Action: Set, State: Working, Turn: turn(p), OnlyFrom: Blocked}
	case "Notification":
		if p.NotificationType == "idle_prompt" {
			// Safety net for turns that ended without Stop, e.g. user interrupts.
			return Transition{Action: Set, State: Idle}
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
		return Transition{Action: Set, State: Blocked, Kind: KindPermission, Turn: turn(p), Tool: p.ToolName}
	case "PostToolUse":
		return Transition{Action: Set, State: Working, Turn: turn(p), OnlyFrom: Blocked, Tool: p.ToolName}
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
