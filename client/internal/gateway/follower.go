package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// machineState is what one machine's watch reported, kept for every page.
type machineState struct {
	name string

	mu       sync.RWMutex
	seq      int64
	sessions map[string]Session
	panes    []Pane
	up       bool
	err      string
}

func newMachineState(name string) *machineState {
	return &machineState{name: name, sessions: map[string]Session{}}
}

func sessionKey(agent, id string) string { return agent + "/" + id }

// replace takes a snapshot as the machine's whole state.
func (m *machineState) replace(seq int64, sessions []Session, panes []Pane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq = seq
	m.sessions = make(map[string]Session, len(sessions))
	for _, s := range sessions {
		m.sessions[sessionKey(s.Agent, s.ID)] = s
	}
	m.panes = append([]Pane(nil), panes...)
}

// setPanes takes the latest pane list.
func (m *machineState) setPanes(panes []Pane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panes = append([]Pane(nil), panes...)
}

// apply records one event and updates the session it reports.
func (m *machineState) apply(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Seq > m.seq {
		m.seq = e.Seq
	}
	k := sessionKey(e.Agent, e.Session)
	switch {
	case e.ended():
		delete(m.sessions, k)
	case e.State != "":
		prev, known := m.sessions[k]
		var since *time.Time
		switch {
		case e.Since != nil:
			at := *e.Since
			since = &at
		case !known || prev.State != e.State || prev.Kind != e.Kind:
			at := e.Time
			since = &at
		default:
			since = prev.Since
		}
		m.sessions[k] = Session{
			LastHookAt: e.LastHookAt, ID: e.Session, Agent: e.Agent, State: e.State, Kind: e.Kind,
			Turn: e.Turn, Since: since, Cwd: e.Cwd, Runtime: e.Runtime, Transcript: e.Transcript,
			Subagents: e.Subagents, Capabilities: e.Capabilities, Requests: e.Requests,
		}
	}
}

// setStatus records whether the machine's watch is up.
func (m *machineState) setStatus(up bool, why string) (changed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed = m.up != up || m.err != why
	m.up, m.err = up, why
	return changed
}

// snapshot returns everything the machine shows now.
func (m *machineState) snapshot() (seq int64, sessions []Session, panes []Pane, up bool, why string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions = make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	slices.SortFunc(sessions, func(a, b Session) int {
		if a.Agent != b.Agent {
			return cmpString(a.Agent, b.Agent)
		}
		return cmpString(a.ID, b.ID)
	})
	return m.seq, sessions, append([]Pane(nil), m.panes...), m.up, m.err
}

// session returns one session as the machine shows it now.
func (m *machineState) session(agent, id string) (Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionKey(agent, id)]
	return s, ok
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// doc is one line of a machine's watch stream.
type doc struct {
	Type     string    `json:"type"`
	Seq      int64     `json:"seq"`
	Sessions []Session `json:"sessions"`
	Panes    []Pane    `json:"panes"`
	Event    *Event    `json:"event"`
}

// follow watches one machine for the life of the gateway, resuming after the
// last sequence number it applied and retrying with backoff.
func (s *Server) follow(m Machine) {
	defer s.wg.Done()
	machine := s.states[m.Name]
	name, _ := json.Marshal(m.Name)
	state := func(up bool, why string) {
		if !machine.setStatus(up, why) {
			return
		}
		body, _ := json.Marshal(map[string]any{"machine": m.Name, "up": up, "error": why})
		s.hub.publish("event: machine\ndata: " + string(body) + "\n\n")
	}
	var since int64 = -1
	for backoff := time.Second; ; backoff = min(2*backoff, 30*time.Second) {
		if s.ctx.Err() != nil {
			return
		}
		run, stop := context.WithCancel(s.ctx)
		args := []string{"watch"}
		if since >= 0 {
			args = append(args, "--since", strconv.FormatInt(since, 10))
		}
		cmd := m.command(run, args...)
		stderr := &capped{}
		cmd.Stderr = stderr
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			stop()
			state(false, exitError(err, stderr.Bytes()))
			if !sleep(s.ctx, backoff) {
				return
			}
			continue
		}
		up, got := false, false
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			line := slices.Clone(sc.Bytes())
			// A line that is no JSON document is dropped: it could
			// otherwise end an event early, \r being a line end too.
			if !json.Valid(line) {
				continue
			}
			var d doc
			if json.Unmarshal(line, &d) != nil {
				continue
			}
			applied := true
			switch d.Type {
			case "snapshot":
				machine.replace(d.Seq, d.Sessions, d.Panes)
				since = d.Seq
				s.track(m.Name, d.Sessions)
			case "panes":
				machine.setPanes(d.Panes)
				s.trackPanes(m.Name, d.Panes)
			case "event":
				if d.Event == nil {
					continue
				}
				machine.apply(*d.Event)
				if d.Event.Seq > since {
					since = d.Event.Seq
				}
				if d.Event.State != "" {
					s.track(m.Name, []Session{{Agent: d.Event.Agent, ID: d.Event.Session}})
				}
				if d.Event.Alert != nil {
					s.alert(m.Name, *d.Event)
				}
			default:
				applied = false
			}
			if !applied {
				continue
			}
			got = true
			if !up {
				up, backoff = true, time.Second
				state(true, "")
			}
			frame := "event: doc\ndata: {\"machine\":" + string(name) + ",\"doc\":" + string(line) + "}\n\n"
			s.hub.publish(frame)
		}
		stop() // a stream left unread would block tmag forever
		err = cmd.Wait()
		if err == nil {
			err = errors.New("tmag watch ended")
		}
		stop()
		if s.ctx.Err() != nil {
			return
		}
		why := exitError(err, stderr.Bytes())
		if !got {
			why = "no watch document"
		}
		state(false, why)
		if !sleep(s.ctx, backoff) {
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// subscriber is one open page stream.
type subscriber struct {
	id         int
	credential string
	frames     chan string
	cancel     context.CancelFunc
}

// hub fans one set of frames out to every open page.
type hub struct {
	mu   sync.Mutex
	next int
	subs map[int]*subscriber
}

func newHub() *hub { return &hub{subs: map[int]*subscriber{}} }

func (h *hub) subscribe(credential string, cancel context.CancelFunc) *subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	sub := &subscriber{id: h.next, credential: credential, frames: make(chan string, 256), cancel: cancel}
	h.subs[sub.id] = sub
	return sub
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[sub.id] != sub {
		return
	}
	delete(h.subs, sub.id)
	close(sub.frames)
}

func (h *hub) publish(frame string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, sub := range h.subs {
		select {
		case sub.frames <- frame:
		default:
			// A page that cannot keep up is disconnected and takes a fresh
			// snapshot when it reconnects.
			delete(h.subs, id)
			close(sub.frames)
			sub.cancel()
		}
	}
}

// next waits for the next frame of one page stream. A cancelled stream,
// whose credential was revoked or which fell behind, writes nothing more,
// even when frames are still buffered.
func next(ctx context.Context, frames <-chan string, ping <-chan time.Time) (string, bool) {
	select {
	case <-ctx.Done():
		return "", false
	case f, ok := <-frames:
		if !ok || ctx.Err() != nil {
			return "", false
		}
		return f, true
	case <-ping:
		if ctx.Err() != nil {
			return "", false
		}
		return "event: ping\ndata: 1\n\n", true
	}
}

// closeAll ends every open page stream.
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, sub := range h.subs {
		delete(h.subs, id)
		sub.cancel()
	}
}

// closeInactive ends streams whose credential is no longer active.
func (h *hub) closeInactive(active map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, sub := range h.subs {
		if !active[sub.credential] {
			delete(h.subs, id)
			sub.cancel()
		}
	}
}

// frame builds one server-sent event.
func frame(event string, v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, b)
}

// track says the machine still shows these sessions.
func (s *Server) track(machine string, sessions []Session) { s.seen.touch(machine, sessions) }

// trackPanes says the machine still shows the sessions its panes show.
func (s *Server) trackPanes(machine string, panes []Pane) {
	sessions := make([]Session, 0, len(panes))
	for _, p := range panes {
		if p.Session != "" {
			sessions = append(sessions, Session{Agent: p.Agent, ID: p.Session})
		}
	}
	s.seen.touch(machine, sessions)
}
