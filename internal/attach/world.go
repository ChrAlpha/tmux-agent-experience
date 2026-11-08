// Package attach decides which panes show which sessions, and keeps the
// pane options tmux displays in step with the sessions they show.
//
// A client is an agent's interactive process in a pane, seen in the process
// table. An attachment says that a client shows a session; it is stored on
// the pane, needs evidence, and ends when the client does, when the pane
// goes away, or when the client is seen showing something else.
package attach

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
// with no live server are skipped. A canceled scan returns no partial world.
// Panes are listed first, so the process table is at least as new as every
// attachment they carry.
func Load(ctx context.Context) (*World, error) {
	w := &World{}
	servers, err := tmux.ServersContext(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		panes, err := s.Panes(ctx)
		if err != nil {
			absent, probeErr := s.Absent(ctx)
			if probeErr != nil || !absent {
				return nil, &WorldUnavailable{Category: "pane_listing"}
			}
			continue
		}
		w.Panes = append(w.Panes, panes...)
	}
	pt, err := proc.SnapshotContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &WorldUnavailable{Category: "process_table"}
	}
	w.Proc = pt
	return w, nil
}

// Client is an agent's interactive process running in a pane.
type Client struct {
	Pane    tmux.Pane
	Agent   string
	Process proc.Process
}

// Foreground reports whether the client holds its pane's terminal, so that
// keys sent to the pane reach it rather than, say, the shell it was
// suspended to.
func (c Client) Foreground() bool { return c.Process.Foreground() }

// ClientIn returns the agent client running in the pane, if any.
func (w *World) ClientIn(p tmux.Pane) (Client, bool) {
	agent, kind, ok := w.Proc.FindAgent(p.PID)
	if !ok {
		return Client{}, false
	}
	return Client{Pane: p, Agent: kind, Process: agent}, true
}

// Clients returns every agent client, in pane order.
func (w *World) Clients() []Client {
	var out []Client
	for _, p := range w.Panes {
		if c, ok := w.ClientIn(p); ok {
			out = append(out, c)
		}
	}
	return out
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

// Enclosing returns the pane whose process tree contains pid, stopping at
// agent services: a process run by a daemon belongs to no pane, whatever
// environment or parentage the daemon inherited.
func (w *World) Enclosing(pid int) (tmux.Pane, bool) {
	byPID := make(map[int]tmux.Pane, len(w.Panes))
	for _, p := range w.Panes {
		byPID[p.PID] = p
	}
	for _, a := range w.Proc.Ancestors(pid) {
		if p, known := w.Proc.Get(a); known && proc.ServiceKind(p) != "" {
			return tmux.Pane{}, false
		}
		if pane, ok := byPID[a]; ok {
			return pane, true
		}
	}
	return tmux.Pane{}, false
}

// ClientOf returns the client that host is, when host is an interactive
// agent process that is its pane's own agent. A hook whose nearest agent is
// nested inside another agent's pane (codex exec run by Claude's Bash tool,
// say) proves nothing about that pane.
func (w *World) ClientOf(host proc.Host) (Client, bool) {
	if host.Role != proc.Terminal {
		return Client{}, false
	}
	pane, ok := w.Enclosing(host.Process.PID)
	if !ok {
		return Client{}, false
	}
	c, ok := w.ClientIn(pane)
	if !ok || c.Agent != host.Kind || c.Process.PID != host.Process.PID {
		return Client{}, false
	}
	return c, true
}

// WorldUnavailable never represents failed discovery as an empty world.
type WorldUnavailable struct{ Category string }

func (e *WorldUnavailable) Error() string { return "world_unavailable: " + e.Category }
