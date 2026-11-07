package query

import (
	"context"
	"fmt"
	"os"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// Caller is who runs this tmag command, as far as the process table proves
// it: the pane it runs in, and the agent whose tool runs it. A person, a
// script or a remote client has no By.
type Caller struct {
	Credential string            `json:"credential,omitempty"`
	Pane       *eventlog.PaneRef `json:"pane,omitempty"`
	By         *eventlog.Actor   `json:"by,omitempty"`
}

// sessionVars are the variables each agent sets for the commands its tools
// run, naming the session (for Codex, the thread: a subagent's own) whose
// call runs them. They are no evidence of their own, as no environment is:
// they only tell apart the sessions of an agent process the caller is
// proven to descend from.
var sessionVars = map[string]string{proc.Claude: "CLAUDE_CODE_SESSION_ID", proc.Codex: "CODEX_THREAD_ID"}

// Caller identifies the process running this command in the world v was
// built from.
func (v View) Caller(w *attach.World) Caller { return identify(v, w, os.Getpid(), os.Getenv) }

// identify finds who process pid is. Its pane is the one whose process tree
// holds it outside any agent service. It is an agent's when an agent client
// or service is among its ancestors; its session is one of those the
// nearest such process that hosts sessions runs: the one the agent's
// variable names, or else the only one. Anything else leaves the session
// unknown.
func identify(v View, w *attach.World, pid int, getenv func(string) string) Caller {
	var c Caller
	if p, ok := w.Enclosing(pid); ok {
		c.Pane = &eventlog.PaneRef{Server: p.Server, ID: p.ID}
	}
	for _, a := range w.Proc.Ancestors(pid) {
		p, ok := w.Proc.Get(a)
		if !ok {
			continue
		}
		kind := proc.ServiceKind(p)
		if kind == "" {
			kind = proc.AgentKind(p)
		}
		if kind == "" {
			continue
		}
		if c.By == nil {
			c.By = &eventlog.Actor{Agent: kind}
		}
		var hosted []session.Record
		for _, r := range v.records {
			if r.Agent == kind && r.Host.PID == p.PID && r.Host.Start == p.Start {
				hosted = append(hosted, r)
			}
		}
		if len(hosted) == 0 {
			continue // a sandbox helper, say, between the tool and its agent
		}
		c.By = &eventlog.Actor{Agent: kind}
		named := getenv(sessionVars[kind])
		for _, r := range hosted {
			switch {
			case named == "" && len(hosted) == 1, named == r.ID:
				c.By.Session = r.ID
			case named != "" && r.ShowsSubagent(named):
				c.By.Session, c.By.Subagent = r.ID, named
			}
		}
		break
	}
	return c
}

// Is reports whether the caller is s's own agent.
func (c Caller) Is(s Session) bool {
	return c.By != nil && c.By.Session != "" && c.By.Agent == s.Agent && c.By.Session == s.ID
}

// selfGate refuses a verb a session's own agent turns on itself: its turn is
// the one running the command.
func (c Caller) selfGate(s Session, verb string) error {
	if c.Is(s) {
		return &RefusedError{RefuseSelf, fmt.Sprintf("%s is the session running this command; an agent does not %s itself", s.ID, verb)}
	}
	return nil
}

// stamp records where an event's verb came from.
func (c Caller) stamp(ev *eventlog.Event) { ev.From, ev.By, ev.Credential = c.Pane, c.By, c.Credential }

// WithOwner carries an authenticated HTTP admission into the common control
// path. CLI callers still derive their identity from the process table.
type ownerKey struct{}

func WithOwner(ctx context.Context, grant auth.Grant) context.Context {
	return context.WithValue(ctx, ownerKey{}, grant)
}
func callerFor(ctx context.Context, v View, w *attach.World) Caller {
	if g, ok := ctx.Value(ownerKey{}).(auth.Grant); ok && g.ID() != "" {
		return Caller{Credential: g.ID()}
	}
	return v.Caller(w)
}

// OwnerGrant returns only the previously authenticated grant carried by ctx.
func OwnerGrant(ctx context.Context) auth.Grant {
	g, _ := ctx.Value(ownerKey{}).(auth.Grant)
	return g
}
