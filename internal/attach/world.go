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
	"slices"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
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
	if err := w.LoadPanes(ctx); err != nil {
		return nil, err
	}
	pt, err := proc.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	w.Proc = pt
	return w, nil
}

// LoadPanes lists the panes of every reachable tmux server into w, keeping
// its process table. Sockets with no live server are skipped. A scan that
// fails leaves w's panes as they were.
func (w *World) LoadPanes(ctx context.Context) error {
	servers, err := tmux.Servers(ctx)
	if err != nil {
		return err
	}
	var panes []tmux.Pane
	for _, s := range servers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p, err := s.Panes(ctx); err == nil {
			panes = append(panes, p...)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.Panes = panes
	return nil
}

// Client is an agent's interactive process running in a pane.
type Client struct {
	Pane    tmux.Pane
	Agent   string
	Process proc.Process
}

// Shows reports whether the client's pane holds the attachment a hook of
// the client's own process makes to rec.
func (c Client) Shows(rec session.Record) bool {
	a, ok := FromPane(c.Pane)
	return ok && a == Attachment{Agent: rec.Agent, Session: rec.ID, PID: c.Process.PID, Start: c.Process.Start, Evidence: Process}
}

// Foreground reports whether the client holds its pane's terminal, so that
// keys sent to the pane reach it rather than, say, the shell it was
// suspended to.
func (c Client) Foreground() bool { return c.Process.Foreground() }

// ClientIn returns the agent client running in the pane, if any: the
// declared agent the pane's attachment names, while that process runs in
// the pane, or else the nearest agent tmag knows by its program. A declared
// agent is known by the process its hooks proved, not by its program, which
// any process may share; Check holds its attachment to its record's host.
func (w *World) ClientIn(p tmux.Pane) (Client, bool) {
	if a, ok := FromPane(p); ok && proc.Declarable(a.Agent) {
		if q, ok := w.within(p, a.PID, a.Start); ok {
			return Client{Pane: p, Agent: a.Agent, Process: q}, true
		}
	}
	agent, kind, ok := w.Proc.FindAgent(p.PID)
	if !ok {
		return Client{}, false
	}
	return Client{Pane: p, Agent: kind, Process: agent}, true
}

// within returns process pid, as it started at start, when it runs in the
// pane: it is the pane's process or one of its descendants.
func (w *World) within(p tmux.Pane, pid int, start string) (proc.Process, bool) {
	q, ok := w.Proc.Get(pid)
	if !ok || q.Uncertain || q.Start != start || !slices.Contains(w.Proc.Ancestors(pid), p.PID) {
		return proc.Process{}, false
	}
	return q, true
}

// Runs reports whether c is still its pane's client in w: tmag finds the
// same client there, or c is a declared agent its pane shows nothing of yet,
// whose process still runs in the pane beside no other client.
func (w *World) Runs(c Client) bool {
	if now, ok := w.ClientIn(c.Pane); ok {
		return now.Agent == c.Agent && now.Process.PID == c.Process.PID && now.Process.Start == c.Process.Start
	}
	_, ok := w.within(c.Pane, c.Process.PID, c.Process.Start)
	return ok && proc.Declarable(c.Agent)
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
// say) proves nothing about that pane. A declared agent is its pane's own
// while its pane holds no other client tmag finds.
func (w *World) ClientOf(host proc.Host) (Client, bool) {
	if host.Role != proc.Terminal {
		return Client{}, false
	}
	pane, ok := w.Enclosing(host.Process.PID)
	if !ok {
		return Client{}, false
	}
	c, ok := w.ClientIn(pane)
	if !ok && proc.Declarable(host.Kind) {
		q, ok := w.within(pane, host.Process.PID, host.Process.Start)
		return Client{Pane: pane, Agent: host.Kind, Process: q}, ok
	}
	if !ok || c.Agent != host.Kind || c.Process.PID != host.Process.PID {
		return Client{}, false
	}
	return c, true
}
