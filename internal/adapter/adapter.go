// Package adapter maps agent hook payloads onto tmag state transitions.
//
// Payload deliberately declares no prompt, message or tool-input fields: content
// is discarded at decode time and can never reach tmux options or the event log.
package adapter

import (
	"encoding/json"
	"fmt"
	"slices"

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
	HookEventName    string `json:"hook_event_name"`
	SessionID        string `json:"session_id"`
	Cwd              string `json:"cwd"`
	Source           string `json:"source"`
	PromptID         string `json:"prompt_id"`
	TurnID           string `json:"turn_id"`
	AgentID          string `json:"agent_id"`
	ToolName         string `json:"tool_name"`
	ToolUseID        string `json:"tool_use_id"`
	NotificationType string `json:"notification_type"`
	// BackgroundTasks lists the tasks a Stop reports still running; nil when
	// the payload does not say.
	BackgroundTasks *[]Task `json:"background_tasks"`
}

// Task is a background task: a shell command, a subagent (whose id is its
// agent_id) or another kind of work.
type Task struct {
	ID string `json:"id"`
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
	Set                  // the main agent's turn began or ended: set state, kind and turn
	Ask                  // a prompt for the user opened; with Call set, its tool call started
	Start                // a tool call started
	Finish               // a tool call finished; see Transition.Call
	Settle               // every tool call of Call.Agent is over: a batch resolved or the agent stopped
	Answer               // a prompt that is not about a tool call was answered
	End                  // the session ended; drop the record
)

// Prompt is something an agent waits on the user for: a tool permission, a
// question or an MCP elicitation, raised by the main agent or a subagent.
type Prompt struct {
	// Agent is the subagent that asked, "" for the main agent.
	Agent string
	// Tool is the tool asked about, or "" when the event does not say; any
	// finished tool answers an unnamed prompt.
	Tool string
}

// Call is a tool call between its start and its end. Permission requests do
// not say which call they are about, so a prompt for a tool stays open while
// any call of that tool by the same agent runs.
type Call struct {
	Agent string // "" for the main agent
	Tool  string
	ID    string
}

// Keep names the agents whose open prompts and running tool calls outlive a
// Set; everyone else's are over.
type Keep struct {
	// Subagents keeps every subagent's: the main agent's turn changed, and
	// subagents run in the background across its turns.
	Subagents bool
	// Running keeps these subagents': the main agent's turn ended while they
	// still run.
	Running []string
}

// Has reports whether agent's prompts and calls outlive the Set.
func (k Keep) Has(agent string) bool {
	return agent != "" && (k.Subagents || slices.Contains(k.Running, agent))
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
	// Call is the tool call a Start or Ask begins or a Finish ends; for a
	// Settle only its Agent is set.
	Call Call
	// Reminder marks an Ask that may repeat a prompt already open: it opens
	// one only during a turn with no prompt pending.
	Reminder bool
	// Keep is whose prompts and calls a Set leaves open.
	Keep Keep
}

// NeedsRecord reports whether the transition only changes an existing
// record: without one it has nothing to act on.
func (t Transition) NeedsRecord() bool {
	switch t.Action {
	case Start, Finish, Settle, Answer:
		return true
	}
	return t.Reminder
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

func call(p Payload) Call { return Call{Agent: p.AgentID, Tool: p.ToolName, ID: p.ToolUseID} }

func start(p Payload) Transition { return Transition{Action: Start, Turn: turn(p), Call: call(p)} }

func finish(p Payload) Transition { return Transition{Action: Finish, Turn: turn(p), Call: call(p)} }

func settle(p Payload) Transition {
	return Transition{Action: Settle, Turn: turn(p), Call: Call{Agent: p.AgentID}}
}

func answer(p Payload, tool string) Transition {
	return Transition{Action: Answer, Turn: turn(p), Prompt: Prompt{Agent: p.AgentID, Tool: tool}}
}

// promptSubmitted starts a main-agent turn; subagents run on across it.
func promptSubmitted(p Payload) Transition {
	return Transition{Action: Set, State: Working, Turn: turn(p), Keep: Keep{Subagents: true}}
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
		return promptSubmitted(p)
	case "PreToolUse":
		if p.ToolName == askUserQuestion {
			tr := ask(p, askUserQuestion)
			tr.Call = call(p)
			return tr
		}
		return start(p)
	case "PermissionRequest":
		// AskUserQuestion also passes through the permission flow; it is
		// still a question for the user, not a tool approval.
		return ask(p, p.ToolName)
	case "Elicitation":
		return ask(p, elicitation)
	case "PostToolUse", "PostToolUseFailure":
		return finish(p)
	case "PostToolBatch", "SubagentStop":
		// A denied call has no PostToolUse; its batch still resolves.
		return settle(p)
	case "ElicitationResult":
		return answer(p, elicitation)
	case "Notification":
		switch p.NotificationType {
		case "idle_prompt":
			// Safety net for turns that ended without Stop, e.g. user
			// interrupts. Claude sends none while a prompt is on screen.
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
		if ids, ok := running(p.BackgroundTasks); ok {
			return Transition{Action: Set, State: Working, Kind: KindBackground, Turn: turn(p), Keep: Keep{Running: ids}}
		}
		return Transition{Action: Set, State: Idle, Turn: turn(p)}
	case "StopFailure":
		return Transition{Action: Set, State: Idle, Kind: KindError, Turn: turn(p), Keep: Keep{Subagents: true}}
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
		return promptSubmitted(p)
	case "PreToolUse":
		return start(p)
	case "PermissionRequest":
		return ask(p, p.ToolName)
	case "PostToolUse":
		// Codex reports successful calls only.
		return finish(p)
	case "PreModelRequest", "SubagentStop":
		// The agent asks the model again once all its calls are over.
		return settle(p)
	case "Stop", "Interrupt":
		return Transition{Action: Set, State: Idle, Turn: turn(p)}
	case "SessionEnd":
		return Transition{Action: End}
	}
	return Transition{}
}

// running returns the ids of the tasks a Stop reports still running, and
// whether there is any.
func running(tasks *[]Task) ([]string, bool) {
	if tasks == nil || len(*tasks) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(*tasks))
	for _, t := range *tasks {
		if t.ID != "" {
			ids = append(ids, t.ID)
		}
	}
	return ids, true
}
