// Package bind decides which tmux pane, if any, an agent session lives in.
package bind

import (
	"context"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// World is a point-in-time view of every tmux server and the process table.
type World struct {
	Panes []tmux.Pane
	Proc  *proc.Table
}

// Load snapshots all reachable tmux servers and the process table. Sockets
// with no live server are skipped; servers not reached before ctx ends are
// left out.
func Load(ctx context.Context) (*World, error) {
	pt, err := proc.Snapshot()
	if err != nil {
		return nil, err
	}
	w := &World{Proc: pt}
	for _, s := range tmux.Servers() {
		if done(ctx) {
			break // out of time: a partial view beats none on a hook path
		}
		if panes, err := s.Panes(ctx); err == nil {
			w.Panes = append(w.Panes, panes...)
		}
	}
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

// Find returns the pane with the given server socket and id.
func (w *World) Find(server, id string) (tmux.Pane, bool) {
	for _, p := range w.Panes {
		if p.Server == server && p.ID == id {
			return p, true
		}
	}
	return tmux.Pane{}, false
}
