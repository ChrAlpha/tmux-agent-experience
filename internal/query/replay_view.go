package query

import (
	"context"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

// ReplayedView applies a snapshot's copied overlap for a one-shot metadata
// display. A watch instead emits the original snapshot and then those events.
// This does not advance Seq, recapture a cursor, discover panes, or prove a
// caller/control channel. Unconfirmed Send reports retain their reported state.
// Work is linear in the image, events and changed bindings, with cancellation
// checks throughout. Session order retains first appearance in the image/log.
func (s Snapshot) ReplayedView(ctx context.Context) (View, error) {
	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	v := View{Panes: make([]Pane, 0, len(s.Panes)), Sessions: []Session{}}
	sessions := map[[2]string]Session{}
	ordered := map[[2]string]bool{}
	var order [][2]string
	remember := func(key [2]string) {
		if !ordered[key] {
			ordered[key] = true
			order = append(order, key)
		}
	}
	for _, rec := range s.Sessions {
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
		key := [2]string{rec.Agent, rec.ID}
		sessions[key] = rec
		remember(key)
	}
	paneIndex := map[[2]string]int{}
	bindings := map[[2]string]map[int]struct{}{}
	bind := func(key [2]string, i int) {
		if bindings[key] == nil {
			bindings[key] = map[int]struct{}{}
		}
		bindings[key][i] = struct{}{}
	}
	for _, p := range s.Panes {
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
		i := len(v.Panes)
		v.Panes = append(v.Panes, p)
		paneIndex[[2]string{p.Server, p.ID}] = i
		if p.Session != "" {
			bind([2]string{p.Agent, p.Session}, i)
		}
	}
	for _, e := range s.Backlog.Events {
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
		key := [2]string{e.Agent, e.Session}
		switch {
		case e.Ended():
			delete(sessions, key)
			for i := range bindings[key] {
				if err := ctx.Err(); err != nil {
					return View{}, err
				}
				p := &v.Panes[i]
				p.Session, p.Evidence, p.View = "", "", ""
			}
			delete(bindings, key)
		case e.State != "":
			prior, known := sessions[key]
			since := e.Since
			if since == nil {
				since = prior.Since
				if !known || prior.State != e.State || prior.Kind != e.Kind {
					at := e.Time
					since = &at
				}
			}
			sessions[key] = Session{ID: e.Session, Agent: e.Agent, State: e.State, Kind: e.Kind, Turn: e.Turn, Since: since, Observation: e.Observation, Cwd: e.Cwd, Runtime: e.Runtime, Transcript: e.Transcript, Subagents: e.Subagents, Capabilities: e.Capabilities, Requests: e.Requests}
			remember(key)
		}
		if e.Pane == nil {
			continue
		}
		i, known := paneIndex[[2]string{e.Pane.Server, e.Pane.ID}]
		if !known {
			continue
		}
		p := &v.Panes[i]
		// Events do not carry command/title/ancestry proof to invent a new pane,
		// change its agent, or infer main vs subagent for a changed attachment.
		if p.Agent != e.Agent {
			continue
		}
		switch e.Event {
		case eventlog.Attach:
			if _, known := sessions[key]; known {
				// Even an equal session/evidence pair omits process identity and
				// title view. An Attach report cannot certify the captured view.
				p.View = ""
				delete(bindings[[2]string{p.Agent, p.Session}], i)
				p.Session, p.Evidence = e.Session, e.Evidence
				bind(key, i)
			}
		case eventlog.Detach:
			if p.Session == e.Session {
				delete(bindings[key], i)
				p.Session, p.Evidence, p.View = "", "", ""
			}
		}
	}
	shown := map[[2]string][]Shown{}
	for _, p := range v.Panes {
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
		if p.Session == "" {
			continue
		}
		key := [2]string{p.Agent, p.Session}
		shown[key] = append(shown[key], Shown{Server: p.Server, ID: p.ID, Evidence: p.Evidence, View: p.View})
	}
	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
		rec, known := sessions[key]
		if !known {
			continue
		}
		rec.Panes = shown[key]
		if rec.Panes == nil {
			rec.Panes = []Shown{}
		}
		v.Sessions = append(v.Sessions, rec)
	}
	return v, ctx.Err()
}
