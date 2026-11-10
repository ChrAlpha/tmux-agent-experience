// Package adapter maps agent hook payloads onto tmag state transitions.
//
// Payload deliberately declares no prompt, message or tool-input fields: content
// is discarded at decode time and can never reach tmux options or the event
// log. It keeps where the agent's transcript is, which is a path.
package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

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
// these field names, and tmag's extensions for Pi and oh-my-pi use them too.
type Payload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	// Source says why a session started, Reason why it ended.
	Source string `json:"source"`
	Reason string `json:"reason"`
	// StopReason is how the last model response of a turn ended, as Pi and
	// oh-my-pi name it ("stop", "aborted", "error", ...).
	StopReason string `json:"stop_reason"`
	// Mode is how Pi or oh-my-pi runs: "tui" for a person at its terminal,
	// or for a program (rpc, json, print).
	Mode             string `json:"mode"`
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

// Bounds of what tmag keeps of a payload. Agents send far less; these keep
// whatever they send from growing a session's record or an event beyond a
// small size, so that nothing downstream has to admit or refuse a change.
const (
	// MaxPath is PATH_MAX: a longer path names no file, and is dropped.
	MaxPath = 4096
	// MaxName bounds names and ids, which are cut, so that a value cut once
	// is cut the same in every hook that reports it.
	MaxName = 256
	// MaxTasks bounds the background tasks a Stop lists.
	MaxTasks = 64
)

// Decode parses a hook payload and bounds what it keeps of it.
func Decode(data []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("decode hook payload: %w", err)
	}
	if p.SessionID == "" || p.HookEventName == "" {
		return p, fmt.Errorf("hook payload lacks session_id or hook_event_name")
	}
	for _, f := range []*string{&p.Cwd, &p.TranscriptPath} {
		if len(*f) > MaxPath {
			*f = ""
		}
	}
	for _, f := range []*string{&p.HookEventName, &p.SessionID, &p.Source, &p.Reason, &p.StopReason, &p.Mode, &p.PromptID, &p.TurnID, &p.AgentID, &p.ToolName, &p.ToolUseID, &p.NotificationType} {
		*f = cut(*f)
	}
	if p.BackgroundTasks != nil {
		tasks := *p.BackgroundTasks
		if len(tasks) > MaxTasks {
			tasks = tasks[:MaxTasks]
		}
		bounded := make([]Task, len(tasks))
		for i, t := range tasks {
			bounded[i] = Task{ID: cut(t.ID)}
		}
		p.BackgroundTasks = &bounded
	}
	return p, nil
}

// cut shortens s to at most MaxName bytes, at a character boundary.
func cut(s string) string {
	if len(s) <= MaxName {
		return s
	}
	n := MaxName
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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

// Pseudo tool names, which start with @, stand for prompts that ask the user
// a question rather than for approval of a tool call.
const (
	elicitation = "@elicitation" // an MCP server's request for input
	dialog      = "@dialog"      // a Pi extension's dialog, waiting during a turn
	ompAsk      = "@ask"         // oh-my-pi's ask tool, whose call asks the user
)

func pseudo(tool string) bool { return strings.HasPrefix(tool, "@") }

// askUserQuestion is Claude Code's tool for asking the user a question.
const askUserQuestion = "AskUserQuestion"

// exitPlanMode is Claude Code's tool for presenting a plan for approval.
const exitPlanMode = "ExitPlanMode"

// Kind is the record kind a prompt blocks with.
func (p Prompt) Kind() string {
	if p.Tool == askUserQuestion || pseudo(p.Tool) {
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
	if pseudo(p.Tool) {
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
	case proc.Pi:
		return mapPi(p)
	case proc.OMP:
		return mapOMP(p)
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

// started starts a session anew: idle, with the turn it last ran when it
// resumes one.
func started(p Payload) Transition {
	if p.Source == "compact" {
		return Transition{} // the same process goes on
	}
	return Transition{Action: Set, State: Idle, Turn: turn(p), Fresh: true}
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

// Pi and oh-my-pi report through tmag's extensions (plugins/pi, plugins/omp),
// which run tmag hook in the order of the agents' own events and under their
// names, with the fields Claude Code's hooks use. A turn is named by the
// entry id of the user message that starts it, as the session file records
// it (see package transcript); message_end reports a user message only, and
// agent_start only a run that none starts, such as an extension's.

func mapPi(p Payload) Transition {
	switch p.HookEventName {
	case "session_start":
		if p.Source == "reload" {
			return Transition{} // the same session goes on, in reloaded extensions
		}
		return started(p)
	case "session_shutdown":
		if p.Reason == "reload" {
			return Transition{}
		}
		return Transition{Action: End}
	case "agent_start", "message_end":
		return promptSubmitted(p)
	case "tool_execution_start":
		return start(p)
	case "tool_execution_end":
		return finish(p)
	case "ui_prompt_start":
		// An extension's dialog during a turn: a permission gate, a question.
		return ask(p, dialog)
	case "ui_prompt_end":
		return answer(p, dialog)
	case "agent_settled":
		// Pi runs no more on its own: the turn is over, aborted or not.
		return settled(p)
	}
	return Transition{}
}

func mapOMP(p Payload) Transition {
	switch p.HookEventName {
	case "session_start", "session_switch":
		// session_switch starts the session the process moved to (/new,
		// /resume, /fork, /branch); the one it left is reported shut down.
		return started(p)
	case "session_shutdown":
		return Transition{Action: End}
	case "agent_start", "message_end":
		return promptSubmitted(p)
	case "tool_execution_start":
		if p.ToolName == "ask" {
			tr := ask(p, ompAsk)
			tr.Call = ompCall(p)
			return tr
		}
		return start(p)
	case "tool_execution_end":
		return Transition{Action: Finish, Turn: turn(p), Call: ompCall(p)}
	case "tool_approval_requested":
		tr := ask(p, p.ToolName)
		tr.Call = call(p)
		return tr
	case "tool_approval_resolved":
		return answer(p, p.ToolName)
	case "agent_end":
		// The extension reports only an end that waits on the user, or on
		// background work whose result starts a run of its own.
		return settled(p)
	}
	return Transition{}
}

// ompCall is the call an oh-my-pi tool event is about; its ask tool asks the
// user.
func ompCall(p Payload) Call {
	c := call(p)
	if c.Tool == "ask" {
		c.Tool = ompAsk
	}
	return c
}

// settled ends a Pi or oh-my-pi turn, failed when its last response did.
func settled(p Payload) Transition {
	tr := Transition{Action: Set, State: Idle, Turn: turn(p)}
	if p.StopReason == "error" {
		tr.Kind = KindError
	}
	return tr
}
