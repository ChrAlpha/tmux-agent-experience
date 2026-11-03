// Package bind decides which tmux pane, if any, an agent session lives in.
package bind

import (
	"context"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexidx"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// World is a point-in-time view of every tmux server and the process table.
type World struct {
	Panes []tmux.Pane
	Proc  *proc.Table
	// Threads names Codex threads for title evidence; nil limits that evidence
	// to thread ids.
	Threads Threads
}

// Load snapshots all reachable tmux servers and the process table. Sockets
// with no live server are skipped; servers not reached before ctx ends are
// left out. Panes are listed first, so the process table is at least as new
// as every record they carry: the agent of a record just written is in it.
func Load(ctx context.Context) (*World, error) {
	w := &World{Threads: &codexThreads{home: codexidx.Home()}}
	for _, s := range tmux.Servers() {
		if done(ctx) {
			break // out of time: a partial view beats none on a hook path
		}
		if panes, err := s.Panes(ctx); err == nil {
			w.Panes = append(w.Panes, panes...)
		}
	}
	pt, err := proc.Snapshot()
	if err != nil {
		return nil, err
	}
	w.Proc = pt
	return w, nil
}

func done(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// AgentIn returns the agent kind running in the pane, or "".
func (w *World) AgentIn(p tmux.Pane) string {
	_, kind, _ := w.Proc.FindAgent(p.PID)
	return kind
}

// Foreground reports whether the pane's agent holds the pane's terminal, so
// that keys sent to the pane reach the agent rather than, say, the shell it
// was suspended to.
func (w *World) Foreground(p tmux.Pane) bool {
	agent, _, ok := w.Proc.FindAgent(p.PID)
	return ok && agent.Foreground()
}

// Find returns the pane with the given server socket and id.
func (w *World) Find(server, id string) (tmux.Pane, bool) {
	for _, p := range w.Panes {
		if p.Server == server && p.ID == id {
			return p, true
		}
	}
	return tmux.Pane{}, false
}

// codexThreads loads the Codex thread index on first use.
type codexThreads struct {
	home  string
	names *codexidx.Names
}

func (c *codexThreads) load() *codexidx.Names {
	if c.names == nil {
		n, err := codexidx.Load(c.home)
		if err != nil {
			n = codexidx.Empty() // an unreadable index proves nothing
		}
		c.names = n
	}
	return c.names
}

func (c *codexThreads) Name(id string) (string, bool)     { return c.load().Name(id) }
func (c *codexThreads) ByName(name string) (string, bool) { return c.load().ByName(name) }
