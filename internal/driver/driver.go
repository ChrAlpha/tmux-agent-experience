// Package driver decides how each control verb reaches a session: the one
// table of which channel carries which verb for each agent and runtime.
//
// Clients see only the verbs (send, interrupt, answer) and their gates; which
// channel carries a verb is tmag's business, chosen here from facts observed
// in each agent (see the agents' hook and protocol behaviour in README.md).
package driver

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

// Mode is how a send asks to be answered.
type Mode int

const (
	// Now delivers at once: an idle session starts a turn, a working one
	// reads the prompt at its next step. No answer is asked for.
	Now Mode = iota
	// Reply delivers at once and asks the receiver to answer with tmag reply.
	Reply
	// Wait starts a turn of its own, whose final message the sender reads
	// with tmag read --last.
	Wait
)

// Route is how a send reaches a session. Channel is tried first; Fallback is
// the pane to try while the session is idle when Channel is an inbox no
// process proves. Either field may be empty.
type Route struct {
	Channel  string
	Fallback string
}

// SendRoute returns how a send from caller reaches a session of agent with
// runtime, idle or not, with the response mode it asks for.
//
// An agent's prompt goes through the session's own message channel in any
// state; only where none exists does it fall back to the pane, and only while
// the session is idle. A person's or owner's prompt keeps the person's
// authority: typed through the pane while idle, and through the session's
// channel while it works. A person's wait keeps the pane path.
func SendRoute(agent, runtime string, caller Caller, mode Mode, idle bool) Route {
	service := runtime == "service"
	switch agent {
	case "claude":
		if service {
			if caller == Person && mode == Wait {
				// A background session has no pane, and a person's wait takes
				// the pane path.
				return Route{}
			}
			// Every background session binds an inbox.
			return Route{Channel: Inbox}
		}
		if caller == Person {
			if mode == Wait || idle {
				return Route{Channel: Pane}
			}
			return Route{Channel: Inbox}
		}
		return Route{Channel: Inbox, Fallback: paneIf(idle)}
	case "codex":
		if service {
			return Route{Channel: Service}
		}
		if idle {
			return Route{Channel: Pane}
		}
		return Route{}
	case "pi", "omp":
		if service {
			// A session serving a program takes input from it alone.
			return Route{}
		}
		if caller == Person {
			if mode == Wait || idle {
				return Route{Channel: Pane}
			}
			return Route{Channel: Inbox}
		}
		return Route{Channel: Inbox, Fallback: paneIf(idle)}
	}
	return Route{}
}

func paneIf(idle bool) string {
	if idle {
		return Pane
	}
	return ""
}

// Channel returns the channel that carries a verb with one channel to a
// session of agent with runtime ("terminal", "service", or "" when its hooks
// did not say), or "" when nothing can. Send has no single channel: SendRoute
// chooses it from the caller, the response mode and the session's state.
func Channel(agent, runtime, verb string) string {
	service := runtime == "service"
	switch agent {
	case "claude":
		switch {
		case verb == Answer:
			// The hook runs wherever the session does, terminal or not.
			return Hook
		case service:
			// A background session has no terminal to prompt or interrupt.
			return ""
		case verb == Interrupt:
			return Pane
		}
		return ""
	case "codex":
		switch {
		case service:
			return Service
		case verb == Interrupt:
			return Pane
		}
		// An embedded app-server listens nowhere, and a dialog's keys cannot
		// be bound to a particular request.
		return ""
	case "pi", "omp":
		switch {
		case service:
			// A session serving a program takes input from it alone.
			return ""
		case verb == Interrupt:
			return Pane
		}
		return ""
	}
	return ""
}

// ReportsInterrupt says whether the agent reports an interrupted turn to its
// hooks. Codex does (Interrupt), and so do Pi and oh-my-pi, whose aborted
// runs end as any other; Claude Code reports nothing, not even its idle
// notification, so tmag records the interrupt it delivered itself.
func ReportsInterrupt(agent string) bool {
	return agent == "codex" || agent == "pi" || agent == "omp"
}

// Capabilities lists the verbs some channel carries to a session of agent
// with runtime. Each verb still checks its own gate when used.
func Capabilities(agent, runtime string) []string {
	out := []string{}
	for _, verb := range Verbs {
		carried := false
		if verb == Send {
			carried = SendRoute(agent, runtime, Agent, Now, true).Channel != ""
		} else {
			carried = Channel(agent, runtime, verb) != ""
		}
		if carried {
			out = append(out, verb)
		}
	}
	return out
}
