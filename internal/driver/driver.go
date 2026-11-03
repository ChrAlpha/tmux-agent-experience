// Package driver decides how each control verb reaches a session: the one
// table of which channel carries which verb for each agent and runtime.
//
// Clients see only the verbs (send, interrupt, answer) and their gates; which
// channel carries a verb is tmag's business, chosen here from facts observed
// in each agent (see the agents' hook and protocol behaviour in README.md).
package driver

// Verbs tmag can carry to a session.
const (
	Send      = "send"      // submit a prompt to an idle session
	Interrupt = "interrupt" // stop the running turn
	Answer    = "answer"    // decide an open permission prompt or question
)

// Verbs lists every verb, in the order capabilities are reported.
var Verbs = []string{Send, Interrupt}

// Channels a verb travels by.
const (
	// Pane types into the one pane that shows the session: the only input of
	// an interactive Claude Code session and of Codex run with --no-daemon.
	Pane = "pane"
	// Service talks to the agent service the session runs in: the Codex
	// daemon's control socket.
	Service = "service"
	// Hook hands the decision to the agent's own hook, which waits for it
	// while the agent's dialog is on screen: Claude Code's PermissionRequest.
	Hook = "hook"
)

// Channel returns the channel that carries verb to a session of agent with
// runtime ("terminal", "service", or "" when its hooks did not say), or ""
// when nothing can.
func Channel(agent, runtime, verb string) string {
	service := runtime == "service"
	switch agent {
	case "claude":
		switch {
		case verb == Answer:
			// The hook runs wherever the session does, terminal or not.
			return Hook
		case service:
			// A background session has no terminal, and its inbox labels
			// messages as from another session.
			return ""
		}
		return Pane
	case "codex":
		switch {
		case service:
			return Service
		case verb == Answer:
			// An embedded app-server listens nowhere, and a dialog's keys
			// cannot be bound to a particular request.
			return ""
		}
		return Pane
	}
	return ""
}

// Capabilities lists the verbs some channel carries to a session of agent
// with runtime. Each verb still checks its own gate when used.
func Capabilities(agent, runtime string) []string {
	out := []string{}
	for _, verb := range Verbs {
		if Channel(agent, runtime, verb) != "" {
			out = append(out, verb)
		}
	}
	return out
}
