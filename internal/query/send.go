package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// PendingWindow is how long a submitted prompt holds off further sends
// without a turn confirming it. Input that starts no turn (a slash command,
// say) releases the session when it passes.
const PendingWindow = 15 * time.Second

func pending(rec session.Record, now time.Time) bool {
	return !rec.Sent.IsZero() && now.Sub(rec.Sent) < PendingWindow
}

// Refusal reasons for Send.
const (
	RefuseUnbound    = "unbound"    // no pane proves it shows the session
	RefuseBusy       = "busy"       // the session is not idle, or a prompt was just sent
	RefuseBackground = "background" // the agent does not hold its terminal
	RefuseSubagent   = "subagent"   // the pane shows a subagent, which takes no input
	RefuseChanged    = "changed"    // the target changed between check and delivery
)

// RefusedError is returned when Send's safety gate rejects delivery.
type RefusedError struct {
	Reason string
	Detail string
}

func (e *RefusedError) Error() string { return fmt.Sprintf("refused (%s): %s", e.Reason, e.Detail) }

// Delivery is what Send delivered to.
type Delivery struct {
	Session Session
	Pane    Pane
	Seq     int64 // the Send event
}

// Send delivers text as one submitted prompt to an idle session through the
// one pane that shows it, and returns where it went.
//
// The gate: a pane proves it shows the session (process or title-id), shows
// its main agent, holds its terminal's foreground, and the session is idle
// with no prompt of tmag's still unconfirmed. Everything is checked again
// under the pane's lock and then the session's, right before typing; the
// session's hooks wait for the same session lock, so the turn this prompt
// starts is logged after the Send event.
func Send(ctx context.Context, log *eventlog.Log, target, text string, from *eventlog.PaneRef) (Delivery, error) {
	text = strings.TrimRight(tmux.SanitizePrompt(text), "\n")
	if strings.TrimSpace(text) == "" {
		return Delivery{}, errors.New("nothing to send")
	}
	store := session.Store{Dir: log.Dir}
	env := attach.Env{Log: log, Store: store}
	// Store what the world proves, so that the attachment checked below is
	// one the pane's lock guards.
	if w, err := attach.Load(ctx); err == nil {
		attach.Reconcile(ctx, env, w)
	}
	v, _, err := Load(ctx, store)
	if err != nil {
		return Delivery{}, err
	}
	t, err := Resolve(v, target)
	if err != nil {
		return Delivery{}, err
	}
	if t.Session == nil {
		return Delivery{}, &RefusedError{RefuseUnbound, fmt.Sprintf("%s in %s shows no session tmag can prove", t.Pane.Agent, t.Pane.ID)}
	}
	s := *t.Session
	if t.Pane == nil {
		if len(s.Panes) == 0 {
			return Delivery{}, &RefusedError{RefuseUnbound, fmt.Sprintf("no pane shows %s", s.ID)}
		}
		return Delivery{}, fmt.Errorf("%w: %s is shown by %d panes; name one", ErrAmbiguous, s.ID, len(s.Panes))
	}
	p := *t.Pane
	if err := gate(s, p, v, time.Now()); err != nil {
		return Delivery{}, err
	}

	d := Delivery{Session: s, Pane: p}
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
			if err := gate(fs, fp, fresh, time.Now()); err != nil {
				return err
			}
			rec, ok, err := store.Get(s.Agent, s.ID)
			if err != nil || !ok {
				return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
			}
			// Hold off other senders before typing: a prompt recorded as sent
			// but never delivered costs a pending window, a prompt delivered
			// but not recorded could be delivered twice. A failure while
			// typing does not say whether tmux typed it, so the mark stays.
			rec.Sent = time.Now().UTC().Truncate(time.Second)
			if err := store.Put(rec); err != nil {
				return err
			}
			if err := (tmux.Server{Socket: fp.Server}).Submit(ctx, fp.ID, text); err != nil {
				return fmt.Errorf("typing into %s failed, perhaps after the prompt was delivered; %s takes no other prompt for %s: %w",
					fp.ID, s.ID, PendingWindow, err)
			}
			// From here on the prompt is delivered: a failure must not read as
			// one a caller could retry.
			ev := eventlog.Event{
				Agent: s.Agent, Session: s.ID, Event: eventlog.Send, Cwd: fp.Cwd,
				Pane: &eventlog.PaneRef{Server: fp.Server, ID: fp.ID}, From: from, Bytes: len(text),
			}
			if err := log.Append(ctx, &ev); err != nil {
				return fmt.Errorf("prompt delivered to %s but not logged; do not send it again: %w", fp.ID, err)
			}
			d.Session, d.Pane, d.Seq = fs, fp, ev.Seq
			return nil
		})
	})
	return d, err
}

// gate applies Send's checks to a session and the pane chosen to show it.
func gate(s Session, p Pane, v View, now time.Time) error {
	if p.View == attach.ViewSubagent {
		return &RefusedError{RefuseSubagent, fmt.Sprintf("%s shows a subagent of %s; switch back to the main agent", p.ID, s.ID)}
	}
	if s.State != adapter.Idle {
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s", s.ID, describe(s.State, s.Kind))}
	}
	for _, r := range v.records {
		if r.Agent == s.Agent && r.ID == s.ID && pending(r, now) {
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s has not started the prompt sent %s ago", s.ID, now.Sub(r.Sent).Round(time.Second))}
		}
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
