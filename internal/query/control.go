package query

import (
	"context"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// Refusal reasons of the control verbs.
const (
	RefuseUnbound     = "unbound"     // no pane proves it shows the session
	RefuseBusy        = "busy"        // the session is not idle, or tmag just acted on it
	RefuseIdle        = "idle"        // the session runs no turn to interrupt
	RefuseBackground  = "background"  // the agent does not hold its terminal
	RefuseSubagent    = "subagent"    // the pane shows a subagent, which takes no input
	RefuseChanged     = "changed"     // the target changed between check and delivery
	RefuseUnsupported = "unsupported" // no channel carries the verb to this session
	RefuseSelf        = "self"        // the session's own agent runs the command
	RefuseAgent       = "agent"       // an agent answers only when @tmag-answer lets agents
)

// RefusedError is returned when a verb's safety gate rejects delivery.
type RefusedError struct {
	Reason string
	Detail string
}

func (e *RefusedError) Error() string { return fmt.Sprintf("refused (%s): %s", e.Reason, e.Detail) }

// Delivery is where a verb went.
type Delivery struct {
	Session Session
	Pane    *Pane  // the pane typed into; nil for other channels
	Via     string // the channel (see package driver)
	Seq     int64  // the event logged for it
}

// prepare stores what the world proves, so that an attachment checked
// afterwards is one the pane's lock guards, and returns the view and who
// runs the verb.
func prepare(ctx context.Context, log *eventlog.Log) (View, Caller, error) {
	store := session.Store{Dir: log.Dir}
	if w, err := attach.Load(ctx); err == nil {
		attach.Reconcile(ctx, attach.Env{Log: log, Store: store}, w)
	}
	v, w, err := Load(ctx, store)
	if err != nil {
		return View{}, Caller{}, err
	}
	return v, v.Caller(w), nil
}

// controlled resolves target to the session a verb acts on and the pane it
// named, if any.
func controlled(v View, target string) (Session, *Pane, error) {
	t, err := Resolve(v, target)
	if err != nil {
		return Session{}, nil, err
	}
	if t.Session == nil {
		return Session{}, nil, &RefusedError{RefuseUnbound, fmt.Sprintf("%s in %s shows no session tmag can prove", t.Pane.Agent, t.Pane.ID)}
	}
	return *t.Session, t.Pane, nil
}

func unsupported(s Session, verb string) error {
	return &RefusedError{RefuseUnsupported, fmt.Sprintf("no channel carries %s to %s %s (%s)", verb, s.Agent, s.ID, runtimeName(s.Runtime))}
}

func runtimeName(r string) string {
	if r == "" {
		return "runtime unknown"
	}
	return r
}

// paneOf is the one pane that shows s: the target's, or the only one.
func paneOf(s Session, named *Pane, v View) (Pane, error) {
	if named != nil {
		return *named, nil
	}
	switch len(s.Panes) {
	case 0:
		return Pane{}, &RefusedError{RefuseUnbound, fmt.Sprintf("no pane shows %s", s.ID)}
	case 1:
		if p, ok := findPane(v, s.Panes[0].Server, s.Panes[0].ID); ok {
			return p, nil
		}
		return Pane{}, &RefusedError{RefuseUnbound, fmt.Sprintf("no pane shows %s", s.ID)}
	}
	return Pane{}, fmt.Errorf("%w: %s is shown by %d panes; name one", ErrAmbiguous, s.ID, len(s.Panes))
}

// stateGate checks that a session is in a state a verb acts on.
type stateGate func(s Session, v View, now time.Time) error

// typing delivers a verb into a pane and returns its event, unlogged, and
// what the delivery made of the session when the agent does not report it
// (nil otherwise). It runs under the pane's lock and the session's, with the
// session as stored.
type typing func(ctx context.Context, s Session, p Pane, rec session.Record) (eventlog.Event, *session.Record, error)

// throughPane delivers a verb through the one pane that shows s. The gate:
// the pane proves it shows the session (process or title-id), shows its main
// agent, the agent holds the terminal's foreground, and the session passes
// the verb's own gate. All of it is checked again under the pane's lock and
// then the session's, right before typing, and the event is logged before
// the locks are released: the session's hooks wait for the same session lock,
// so what the verb causes is logged after it.
func throughPane(ctx context.Context, log *eventlog.Log, v View, s Session, named *Pane, gate stateGate, deliver typing) (Delivery, error) {
	p, err := paneOf(s, named, v)
	if err != nil {
		return Delivery{}, err
	}
	if err := paneGate(s, p, v); err != nil {
		return Delivery{}, err
	}
	if err := gate(s, v, time.Now()); err != nil {
		return Delivery{}, err
	}
	store := session.Store{Dir: log.Dir}
	var d Delivery
	err = attach.Lock(ctx, log.Dir, p.Server, p.ID, func() error {
		return store.Lock(ctx, s.Agent, s.ID, func() error {
			// Again, against fresh state: the agent may have started a turn,
			// exited, switched threads or received another prompt since.
			fresh, _, err := Load(ctx, store)
			if err != nil {
				return err
			}
			fp, ok := findPane(fresh, p.Server, p.ID)
			if !ok || fp.Session != s.ID || fp.Agent != s.Agent || !stored(fresh, fp) {
				return &RefusedError{RefuseChanged, fmt.Sprintf("%s no longer shows %s", p.ID, s.ID)}
			}
			fs, ok := fresh.Session(s.Agent, s.ID)
			if !ok {
				return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
			}
			if err := paneGate(fs, fp, fresh); err != nil {
				return err
			}
			if err := gate(fs, fresh, time.Now()); err != nil {
				return err
			}
			rec, ok, err := store.Current(ctx, log, s.Agent, s.ID)
			if err != nil || !ok {
				return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
			}
			ev, after, err := deliver(ctx, fs, fp, rec)
			if err != nil {
				return err
			}
			ev.Agent, ev.Session, ev.Via = fs.Agent, fs.ID, driver.Pane
			ev.Pane = &eventlog.PaneRef{Server: fp.Server, ID: fp.ID}
			if after != nil {
				if _, err := store.Report(ctx, log, *after, &ev); err != nil {
					return fmt.Errorf("%s delivered to %s but not logged; do not repeat it: %w", ev.Event, fp.ID, err)
				}
			} else if err := log.Append(ctx, &ev); err != nil {
				return fmt.Errorf("%s delivered to %s but not logged; do not repeat it: %w", ev.Event, fp.ID, err)
			}
			d = Delivery{Session: fs, Pane: &fp, Via: driver.Pane, Seq: ev.Seq}
			return nil
		})
	})
	return d, err
}

// paneGate applies the pane's checks: it shows the session's main agent,
// and that agent holds its terminal's foreground.
func paneGate(s Session, p Pane, v View) error {
	if p.View == attach.ViewSubagent {
		return &RefusedError{RefuseSubagent, fmt.Sprintf("%s shows a subagent of %s; switch back to the main agent", p.ID, s.ID)}
	}
	for _, l := range v.links {
		if l.Client.Pane.Server == p.Server && l.Client.Pane.ID == p.ID && !l.Client.Foreground() {
			return &RefusedError{RefuseBackground, fmt.Sprintf("%s in %s is not in the foreground; %s is", p.Agent, p.ID, p.Command)}
		}
	}
	return nil
}

func findPane(v View, server, id string) (Pane, bool) {
	for _, p := range v.Panes {
		if p.Server == server && p.ID == id {
			return p, true
		}
	}
	return Pane{}, false
}

// stored reports whether the pane's link to its session is the attachment
// stored on the pane, which the pane's lock guards.
func stored(v View, p Pane) bool {
	for _, l := range v.links {
		if l.Client.Pane.Server == p.Server && l.Client.Pane.ID == p.ID {
			return l.Stored
		}
	}
	return false
}

func describe(state, kind string) string {
	if kind != "" {
		return state + " (" + kind + ")"
	}
	return state
}
