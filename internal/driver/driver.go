// Package driver decides how each control verb reaches a session: the one
// table of which channel carries which verb for each agent and runtime.
//
// Clients see only the verbs (send, interrupt, answer) and their gates; which
// channel carries a verb is tmag's business, chosen here from facts observed
// in each agent (see the agents' hook and protocol behaviour in README.md).
package driver

import "github.com/ChrAlpha/tmux-agent-experience/internal/proc"

// Verbs tmag can carry to a session.
const (
	Send      = "send"      // deliver a prompt now, or as a turn of its own
	Interrupt = "interrupt" // stop the running turn
	Answer    = "answer"    // decide an open permission prompt or question
)

// Verbs lists every verb, in the order capabilities are reported.
var Verbs = []string{Send, Interrupt, Answer}

// Channels a verb travels by.
const (
	// Pane types into the one pane that shows the session: the only input of
	// an interactive Claude Code, Pi or oh-my-pi session and of Codex run
	// with --no-daemon.
	Pane = "pane"
	// Service talks to the agent service the session runs in: the Codex
	// daemon's control socket.
	Service = "service"
	// Hook hands the decision to the agent's own hook, which waits for it
	// while the agent's dialog is on screen: Claude Code's PermissionRequest.
	Hook = "hook"
	// Inbox writes on the socket the session's own agent serves for messages:
	// Claude Code's cross-session inbox, or tmag's extension in Pi and
	// oh-my-pi. The agent queues the message, so it takes one in any state.
	Inbox = "inbox"
)

// Caller is who sends a prompt: a person or an owner client, or an agent's
// tool.
type Caller int

const (
	Person Caller = iota
	Agent
)

// service is the runtime of a session in a background service, which no
// pane shows.
const service = string(proc.Service)

// Plan is how a verb reaches a session: the channels to try, in order, each
// next one when the previous cannot be proven to reach the session. When
// none can, Busy says the session has a pane that takes the verb once it is
// idle; otherwise the last channel's refusal stands, or, with none to try,
// the verb is unsupported.
type Plan struct {
	Try  []string
	Busy bool
}

// SendPlan is how a prompt from caller reaches a session of agent with
// runtime, idle or not, as a turn of its own (wait) or not.
//
// An agent's prompt goes through the session's own message channel in any
// state, and through the pane while the session is idle. A person's or
// owner's prompt keeps the person's authority: typed through the pane while
// idle or for a turn of its own, and through the session's channel while it
// works. A session in a service has no pane: a Codex daemon takes prompts
// itself, Claude Code's supervisor through the session's inbox, and Pi or
// oh-my-pi serving a program none.
func SendPlan(agent, runtime string, caller Caller, wait, idle bool) Plan {
	if runtime == service {
		switch {
		case agent == proc.Codex:
			return Plan{Try: []string{Service}}
		case agent == proc.Claude && !(caller == Person && wait):
			return Plan{Try: []string{Inbox}}
		}
		return Plan{}
	}
	var try []string
	switch agent {
	case proc.Codex:
		if idle {
			try = []string{Pane}
		}
	case proc.Claude, proc.Pi, proc.OMP:
		switch {
		case caller == Person && (wait || idle):
			try = []string{Pane}
		case caller == Person:
			try = []string{Inbox}
		case idle:
			try = []string{Inbox, Pane}
		default:
			try = []string{Inbox}
		}
	default:
		return Plan{}
	}
	return Plan{Try: try, Busy: !idle}
}

// InterruptPlan is how an interrupt reaches a session of agent with
// runtime: the Codex daemon interrupts its own turns, Pi's extension aborts
// a turn and the pane takes Escape where it cannot, and every other
// interactive agent takes Escape in its pane.
func InterruptPlan(agent, runtime string) Plan {
	if runtime == service {
		if agent == proc.Codex {
			return Plan{Try: []string{Service}}
		}
		return Plan{}
	}
	switch agent {
	case proc.Pi:
		return Plan{Try: []string{Inbox, Pane}}
	case proc.Claude, proc.Codex, proc.OMP:
		return Plan{Try: []string{Pane}}
	}
	return Plan{}
}

// AnswerChannel carries an answer to a session of agent with runtime, or is
// "": Claude Code's answer hook, wherever the session runs, and the Codex
// daemon for its approvals.
func AnswerChannel(agent, runtime string) string {
	switch {
	case agent == proc.Claude:
		return Hook
	case agent == proc.Codex && runtime == service:
		return Service
	}
	return ""
}

// ReportsInterrupt says whether the agent reports an interrupted turn to its
// hooks. Codex does (Interrupt), and so do Pi and oh-my-pi, whose aborted
// runs end as any other; Claude Code reports nothing, not even its idle
// notification, so tmag records the interrupt it delivered itself.
func ReportsInterrupt(agent string) bool {
	return agent == proc.Codex || agent == proc.Pi || agent == proc.OMP
}

// Capabilities are the verbs some channel carries to a session of agent with
// runtime, in Verbs order.
func Capabilities(agent, runtime string) []string {
	out := []string{}
	if len(SendPlan(agent, runtime, Agent, false, true).Try) > 0 {
		out = append(out, Send)
	}
	if len(InterruptPlan(agent, runtime).Try) > 0 {
		out = append(out, Interrupt)
	}
	if AnswerChannel(agent, runtime) != "" {
		out = append(out, Answer)
	}
	return out
}
