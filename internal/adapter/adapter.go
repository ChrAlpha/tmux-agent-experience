// Package adapter maps agent hook payloads onto tmag state transitions.
//
// Payload deliberately declares no prompt, message or tool-input fields: content
// is discarded at decode time and can never reach tmux options or the event
// log. It keeps where the agent's transcript is, which is a path.
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
	KindPlan       = "plan"       // a request to approve a plan (blocks as permission)
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
	// TranscriptPath is where the agent writes the session's transcript: a
	// path, not content.
	TranscriptPath string `json:"transcript_path"`
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

// Action says what a transition does to the session.
type Action int

const (
	Ignore Action = iota // event carries no state information
	Set                  // the main agent's turn began or ended: set its state, kind and turn
	Ask                  // a prompt for the user opened; with Call set, its tool call started
	Start                // a tool call started
	Finish               // a tool call finished; see Transition.Call
	Settle               // every tool call of Call.Agent is over: a batch resolved or the agent asks the model again
	Answer               // a prompt that is not about a tool call was answered
	Spawn                // subagent Call.Agent started; it may outlive the main agent's turn
	Reap                 // subagent Call.Agent stopped, and with it its calls and prompts
	End                  // the session ended
)

// Prompt is something an agent waits on the user for: a tool permission, a
// question or an MCP elicitation, raised by the main agent or a subagent.
type Prompt struct {
	// Agent is the subagent that asked, "" for the main agent.
	Agent string `json:"agent,omitempty"`
	// Tool is the tool asked about, or "" when the event does not say; any
	// finished tool answers an unnamed prompt.
	Tool string `json:"tool,omitempty"`
}

// Call is a tool call between its start and its end. Permission requests do
// not say which call they are about, so a prompt for a tool stays open while
// any call of that tool by the same agent runs.
type Call struct {
	Agent string `json:"agent,omitempty"` // "" for the main agent
	Tool  string `json:"tool,omitempty"`
	ID    string `json:"id,omitempty"`
}

// Pseudo tool names for prompts that are not tool calls.
const elicitation = "@elicitation"

// askUserQuestion is Claude Code's tool for asking the user a question.
const askUserQuestion = "AskUserQuestion"

// exitPlanMode is Claude Code's tool for presenting a plan for approval.
const exitPlanMode = "ExitPlanMode"

// Kind is the record kind a prompt blocks with.
func (p Prompt) Kind() string {
	if p.Tool == askUserQuestion || p.Tool == elicitation {
		return KindQuestion
	}
	return KindPermission
}

// RequestKind is what a prompt asks the user to do: approve a tool call
// (permission), answer a question, or approve a plan.
func (p Prompt) RequestKind() string {
	if p.Tool == exitPlanMode {
		return KindPlan
	}
	return p.Kind()
}

// ToolName is the tool the prompt is about, "" when it names none or is not
// about a tool.
func (p Prompt) ToolName() string {
	if p.Tool == elicitation {
		return ""
	}
	return p.Tool
}

// Transition is the effect of one hook event.
type Transition struct {
	Action Action
	State  string
	Kind   string
	// Turn is the turn the event belongs to: the main agent's for main-agent
	// events, the subagent's own for a Codex subagent's.
	Turn string
	// Prompt is the prompt an Ask opens or an Answer closes.
	Prompt Prompt
	// Call is the tool call a Start or Ask begins or a Finish ends; for a
	// Settle, Spawn or Reap only its Agent is set.
	Call Call
	// Reminder marks an Ask that may repeat a prompt already open: it opens
	// one only during a turn with no prompt pending.
	Reminder bool
	// KeepSubagents keeps every subagent's prompts and calls across a Set;
	// otherwise only those of subagents still running in the background
	// survive it.
	KeepSubagents bool
	// Background, when not nil, is the complete list of background work
	// still running after a Set (subagents and other tasks, by id), replacing
	// what the session knew.
	Background *[]string
	// Fresh marks a Set that starts a session process anew: nothing it ran
	// before is still running.
	Fresh bool
	// Late marks a Set from an event that may arrive after the next turn has
	// begun: it applies only while the session is still in Turn.
	Late bool
	// Request names the prompt an Ask opens, so it can be answered; Waits
	// says an answer hook waits under that name (see package answer).
	Request string
	Waits   bool
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

func spawn(p Payload) Transition {
	if p.AgentID == "" {
		return Transition{}
	}
	return Transition{Action: Spawn, Turn: turn(p), Call: Call{Agent: p.AgentID}}
}

func reap(p Payload) Transition {
	if p.AgentID == "" {
		return Transition{}
	}
	return Transition{Action: Reap, Turn: turn(p), Call: Call{Agent: p.AgentID}}
}

func started(p Payload) Transition {
	if p.Source == "compact" {
		return Transition{} // the same process goes on
	}
	return Transition{Action: Set, State: Idle, Fresh: true}
}

func answer(p Payload, tool string) Transition {
	return Transition{Action: Answer, Turn: turn(p), Prompt: Prompt{Agent: p.AgentID, Tool: tool}}
}

// promptSubmitted starts a main-agent turn; subagents run on across it.
func promptSubmitted(p Payload) Transition {
	return Transition{Action: Set, State: Working, Turn: turn(p), KeepSubagents: true}
}

func mapClaude(p Payload) Transition {
	if subagentLifecycle(p) {
		return Transition{}
	}
	switch p.HookEventName {
	case "SessionStart":
		return started(p)
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
	case "PostToolBatch":
		// A denied call has no PostToolUse; its batch still resolves.
		return settle(p)
	case "SubagentStart":
		return spawn(p)
	case "SubagentStop":
		return reap(p)
	case "ElicitationResult":
		return answer(p, elicitation)
	case "Notification":
		switch p.NotificationType {
		case "idle_prompt":
			// Safety net for turns that ended without Stop, e.g. user
			// interrupts. Claude sends none while a prompt is on screen, and
			// sends it a minute late, asynchronously: by then another turn
			// may run.
			return Transition{Action: Set, State: Idle, Turn: turn(p), Late: true}
		case "permission_prompt":
			// A prompt left open for a few seconds. It is the only signal for a
			// sandboxed command's network request, which raises no
			// PermissionRequest; for a tool it repeats one already pending.
			tr := ask(p, "")
			tr.Reminder = true
			return tr
		}
	case "Stop":
		return Transition{Action: Set, State: Idle, Turn: turn(p), Background: tasks(p.BackgroundTasks)}
	case "StopFailure":
		return Transition{Action: Set, State: Idle, Kind: KindError, Turn: turn(p), KeepSubagents: true}
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
		return started(p)
	case "UserPromptSubmit":
		return promptSubmitted(p)
	case "PreToolUse":
		return start(p)
	case "PermissionRequest":
		return ask(p, p.ToolName)
	case "PostToolUse":
		// Codex reports successful calls only.
		return finish(p)
	case "PreModelRequest":
		// The agent asks the model again: whatever it asked the user is
		// settled. Calls moved to a background terminal may still run.
		return settle(p)
	case "SubagentStart":
		return spawn(p)
	case "SubagentStop":
		return reap(p)
	case "Stop", "Interrupt":
		// Stop lists no background work; running subagents stay tracked.
		return Transition{Action: Set, State: Idle, Turn: turn(p)}
	case "SessionEnd":
		return Transition{Action: End}
	}
	return Transition{}
}

// tasks returns the ids of the background tasks a Stop reports still
// running, an empty list when it reports none, or nil when it does not say.
func tasks(list *[]Task) *[]string {
	if list == nil {
		return nil
	}
	ids := []string{}
	for _, t := range *list {
		if t.ID != "" {
			ids = append(ids, t.ID)
		}
	}
	return &ids
}
