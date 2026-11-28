package hook

import (
	"path/filepath"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// claudeInboxVar is where Claude Code exports the path of the session's
// cross-session inbox to its hooks and Bash commands.
const claudeInboxVar = "CLAUDE_CODE_MESSAGING_SOCKET"

// inboxOf is the socket path a hook's agent named for its inbox: Claude Code
// exports it to the hook, Pi's and oh-my-pi's extensions and declared agents
// send it in every payload. A hook that names none records nothing new.
func inboxOf(agent string, p adapter.Payload, getenv func(string) string) string {
	switch {
	case agent == proc.Claude:
		return inboxPath(getenv(claudeInboxVar))
	case agent == proc.Pi, agent == proc.OMP, proc.Declarable(agent):
		return inboxPath(p.Inbox)
	}
	return ""
}

// inboxPath keeps a socket path that could name an inbox: absolute, within
// the platform's limit, and without the optional prefix Claude Code uses.
func inboxPath(s string) string {
	s = strings.TrimPrefix(s, "uds:")
	if s == "" || !filepath.IsAbs(s) || len(s) > adapter.MaxPath {
		return ""
	}
	return s
}

// inboxFor is the inbox a hook proves: the socket its agent named, served by
// the nearest process above the hook that runs the agent's program, whatever
// its role: a background session's listener is a worker, not the service host
// the session records. The process is the one that set the variable or ran
// the extension, so a message delivered to its listener is one the session's
// agent takes. A declared agent's is its host, the process its hook proved.
func inboxFor(t *proc.Table, pid int, agent, path string) *session.Inbox {
	if path == "" || t == nil {
		return nil
	}
	for _, a := range t.Ancestors(pid) {
		p, ok := t.Get(a)
		if !ok {
			continue
		}
		if proc.ProgramKind(p) == agent || proc.Declarable(agent) && proc.ProgramName(p) == agent {
			return &session.Inbox{Path: path, PID: p.PID, Start: p.Start}
		}
	}
	return nil
}
