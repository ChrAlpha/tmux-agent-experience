// Package query implements the read and control operations of the tmag
// contract over sessions, the panes that show them, and the event log.
package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Shown is a pane that shows a session, from the session's side.
type Shown struct {
	Server   string `json:"server"`
	ID       string `json:"id"`
	Evidence string `json:"evidence"`
	View     string `json:"view"`
}

// Session is one agent session, wherever it runs.
type Session struct {
	ID    string     `json:"id"`
	Agent string     `json:"agent"`
	State string     `json:"state"`
	Kind  string     `json:"kind,omitempty"`
	Turn  string     `json:"turn,omitempty"`
	Since *time.Time `json:"since,omitempty"`
	Cwd   string     `json:"cwd,omitempty"`
	// Runtime is how the session runs: in a terminal's own agent process, or
	// in a background service; "" when its hooks did not say.
	Runtime string `json:"runtime,omitempty"`
	// Transcript is where the agent writes the session's transcript.
	Transcript *eventlog.Transcript `json:"transcript,omitempty"`
	// Subagents are the session's running subagents.
	Subagents []string `json:"subagents,omitempty"`
	// Panes are the panes that show the session.
	Panes []Shown `json:"panes"`
}

// Pane is an agent client in a tmux pane and the session it shows, if any.
type Pane struct {
	Server  string `json:"server"`
	ID      string `json:"id"`
	Target  string `json:"target"`
	Cwd     string `json:"cwd"`
	Command string `json:"command"`
	Agent   string `json:"agent"`
	// Session is the id of the session the pane shows; empty when tmag has
	// no proof of one.
	Session  string `json:"session,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	View     string `json:"view,omitempty"`
}

// View is every session and every agent pane at one moment.
type View struct {
	Sessions []Session `json:"sessions"`
	Panes    []Pane    `json:"panes"`

	links   []attach.Link
	records []session.Record
}

// Build computes the view from the world and the stored sessions. It writes
// nothing: it shows what reconciliation would make of the world.
func Build(w *attach.World, recs []session.Record) View {
	live, _ := attach.Live(w, recs)
	v := View{Sessions: []Session{}, Panes: []Pane{}, records: live}
	v.links = attach.Links(w, live)
	shown := map[[2]string][]Shown{}
	for _, l := range v.links {
		p := l.Client.Pane
		pane := Pane{Server: p.Server, ID: p.ID, Target: p.Target(), Cwd: p.Path, Command: p.Command, Agent: l.Client.Agent}
		if l.Session != nil {
			pane.Session, pane.Evidence, pane.View = l.Session.ID, l.Evidence, l.View
			key := [2]string{l.Session.Agent, l.Session.ID}
			shown[key] = append(shown[key], Shown{Server: p.Server, ID: p.ID, Evidence: l.Evidence, View: l.View})
		}
		v.Panes = append(v.Panes, pane)
	}
	for _, r := range live {
		s := Session{
			ID: r.ID, Agent: r.Agent, State: r.State(), Kind: r.Kind(), Turn: r.Turn,
			Cwd: r.Cwd, Runtime: string(r.Host.Role), Transcript: r.TranscriptRef(), Subagents: r.RunningSubagents(),
			Panes: shown[[2]string{r.Agent, r.ID}],
		}
		if s.Panes == nil {
			s.Panes = []Shown{}
		}
		if !r.Since.IsZero() {
			since := r.Since
			s.Since = &since
		}
		v.Sessions = append(v.Sessions, s)
	}
	sort.SliceStable(v.Panes, func(i, j int) bool {
		a, b := v.Panes[i], v.Panes[j]
		if a.Server != b.Server {
			return a.Server < b.Server
		}
		return a.Target < b.Target
	})
	sort.SliceStable(v.Sessions, func(i, j int) bool {
		a, b := v.Sessions[i], v.Sessions[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.ID < b.ID
	})
	return v
}

// Session returns the session with the given agent and id.
func (v View) Session(agent, id string) (Session, bool) {
	for _, s := range v.Sessions {
		if s.Agent == agent && s.ID == id {
			return s, true
		}
	}
	return Session{}, false
}

// Load reads the world and the sessions and builds the view.
func Load(ctx context.Context, store session.Store) (View, *attach.World, error) {
	w, err := attach.Load(ctx)
	if err != nil {
		return View{}, nil, err
	}
	recs, err := store.All()
	if err != nil {
		return View{}, nil, err
	}
	return Build(w, recs), w, nil
}

// Snapshot is the full current state, consistent with events after Seq.
type Snapshot struct {
	Seq int64 `json:"seq"`
	View
}

// TakeSnapshot reads the sequence number before the state, so replaying
// events after Seq over the snapshot never loses an update.
func TakeSnapshot(ctx context.Context, log *eventlog.Log) (Snapshot, error) {
	seq, err := log.Seq()
	if err != nil {
		return Snapshot{}, err
	}
	v, _, err := Load(ctx, session.Store{Dir: log.Dir})
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Seq: seq, View: v}, nil
}

// Errors returned by target resolution and waiting.
var (
	ErrNotFound  = errors.New("no such agent")
	ErrAmbiguous = errors.New("ambiguous target")
	ErrEnded     = errors.New("agent session ended")
)

var paneSpec = regexp.MustCompile(`^(?:(.+):)?(%\d+)$`)

// minIDPrefix is the shortest id prefix accepted as a target.
const minIDPrefix = 4

// Target is what a command addresses: a session, a pane, or both when the
// pane shows the session.
type Target struct {
	Session *Session
	Pane    *Pane
}

// Resolve finds what target names: a pane id (%N, on the caller's server),
// server:%N where server is a socket name or path, or a session id or a
// unique prefix of one. A session shown by exactly one pane resolves to both.
func Resolve(v View, target string) (Target, error) {
	if m := paneSpec.FindStringSubmatch(target); m != nil {
		server := tmux.DefaultServer().Socket
		if m[1] != "" {
			server = tmux.ServerByName(m[1]).Socket
		}
		for i := range v.Panes {
			p := &v.Panes[i]
			if p.Server != server || p.ID != m[2] {
				continue
			}
			t := Target{Pane: p}
			if p.Session != "" {
				if s, ok := v.Session(p.Agent, p.Session); ok {
					t.Session = &s
				}
			}
			return t, nil
		}
		return Target{}, fmt.Errorf("%w in pane %s", ErrNotFound, target)
	}
	if len(target) < minIDPrefix {
		return Target{}, fmt.Errorf("%w: id prefix %q is shorter than %d characters", ErrNotFound, target, minIDPrefix)
	}
	var matches []int
	for i, s := range v.Sessions {
		if strings.HasPrefix(s.ID, target) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return Target{}, fmt.Errorf("%w: %s", ErrNotFound, target)
	case 1:
		s := v.Sessions[matches[0]]
		t := Target{Session: &s}
		if len(s.Panes) == 1 {
			for i := range v.Panes {
				if v.Panes[i].Server == s.Panes[0].Server && v.Panes[i].ID == s.Panes[0].ID {
					t.Pane = &v.Panes[i]
				}
			}
		}
		return t, nil
	}
	return Target{}, fmt.Errorf("%w: %s matches %d sessions", ErrAmbiguous, target, len(matches))
}

// Sender returns the pane the calling process runs in, if any, for provenance.
func Sender(w *attach.World) *eventlog.PaneRef {
	if p, ok := w.Enclosing(os.Getpid()); ok {
		return &eventlog.PaneRef{Server: p.Server, ID: p.ID}
	}
	return nil
}

// SessionEvents returns the most recent events of one session, oldest first.
func SessionEvents(ctx context.Context, log *eventlog.Log, id string, limit int) ([]eventlog.Event, error) {
	all, _, err := log.Since(ctx, 0)
	if err != nil {
		return nil, err
	}
	var out []eventlog.Event
	for _, e := range all {
		if e.Session == id {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// IsAgentKind reports whether s names a supported agent.
func IsAgentKind(s string) bool { return s == proc.Claude || s == proc.Codex }
