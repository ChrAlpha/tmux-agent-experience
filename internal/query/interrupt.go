package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// HaltWindow is how long an interrupt holds off another while the agent
// reports nothing. Claude Code reports nothing on an interrupt until its idle
// notification, and a second Escape on an empty composer opens a menu.
const HaltWindow = 90 * time.Second

func halting(rec session.Record, now time.Time) bool {
	return !rec.Halted.IsZero() && now.Sub(rec.Halted) < HaltWindow
}

// Interrupt stops the main agent's running turn, by the channel the
// session's driver uses, and returns where it went: Pi's own inbox when it
// is proven, otherwise the pane, and the Codex daemon's service.
//
// The session must be working on a turn of its main agent or blocked on a
// prompt (which an interrupt declines), not interrupted by tmag since the
// agent last reported, and not the caller's own. The Halt event is logged
// under the session's lock.
// What the agent does next comes from its hooks: Codex reports the interrupt,
// Pi and oh-my-pi the end of the aborted turn, Claude Code nothing until it
// notices it is idle. A blocked Pi or oh-my-pi declines its dialog on
// Escape, and the agent decides whether its turn goes on; Pi's inbox aborts
// the turn, a dialog layer included, when its extension has an abort.
func Interrupt(ctx context.Context, log *eventlog.Log, target string) (Delivery, error) {
	v, caller, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	s, named, err := controlled(ctx, v, target)
	if err != nil {
		return Delivery{}, err
	}
	if err := caller.selfGate(s, "interrupt"); err != nil {
		return Delivery{}, err
	}
	route := driver.InterruptRoute(s.Agent, s.Runtime)
	if route.Channel == driver.Inbox {
		d, err := throughInboxInterrupt(ctx, log, v, s, caller)
		if err == nil {
			return d, nil
		}
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != RefuseUnbound || route.Fallback != driver.Pane {
			return Delivery{}, err
		}
		route.Channel = driver.Pane
	}
	switch route.Channel {
	case driver.Service:
		var turn string
		return throughService(ctx, log, v, s, serviceVerb{
			gate: runningGate,
			check: func(ctx context.Context, c *codexsvc.Client) error {
				return threadIn(ctx, c, s, RefuseIdle, codexsvc.StatusActive)
			},
			mark: func(rec *session.Record) eventlog.Event {
				rec.Halted, turn = time.Now().UTC().Truncate(time.Second), rec.Turn
				ev := eventlog.Event{Event: eventlog.Halt, Cwd: rec.Cwd}
				caller.stamp(&ev)
				return ev
			},
			deliver: func(ctx context.Context, c *codexsvc.Client) error {
				return c.InterruptTurn(ctx, s.ID, turn)
			},
		})
	case driver.Pane:
		return throughPane(ctx, log, v, s, named, runningGate, paneVerb{
			plan: func(s Session, p Pane, rec session.Record) (session.Record, eventlog.Event, *session.Record) {
				now := time.Now()
				rec.Halted = now.UTC().Truncate(time.Second)
				ev := eventlog.Event{Event: eventlog.Halt, Cwd: p.Cwd}
				caller.stamp(&ev)
				if driver.ReportsInterrupt(s.Agent) {
					return rec, ev, nil
				}
				// The agent reports nothing of it: its turn is over, and with it
				// whatever it asked the user.
				after, _ := session.Apply(rec, adapter.Transition{Action: adapter.Set, State: adapter.Idle, Turn: rec.Turn, Synthetic: true}, now)
				after.Halted = rec.Halted
				return rec, ev, &after
			},
			press: func(ctx context.Context, _ Session, p Pane) error {
				if err := (tmux.Server{Socket: p.Server}).Interrupt(ctx, p.ID); err != nil {
					return fmt.Errorf("pressing Escape in %s failed: %w", p.ID, err)
				}
				return nil
			},
		})
	}
	return Delivery{}, unsupported(s, driver.Interrupt)
}

// throughInboxInterrupt stops s's turn through the inbox its own agent
// serves. The inbox is dialled and proven before the Halt is recorded, and
// the record is marked under the session's lock against fresh state before
// the extension is asked; the extension checks the session again at delivery.
func throughInboxInterrupt(outer context.Context, log *eventlog.Log, v View, s Session, caller Caller) (Delivery, error) {
	if err := runningGate(s, v, time.Now()); err != nil {
		return Delivery{}, err
	}
	store := session.Store{Dir: log.Dir}
	rec, ok, err := store.Get(outer, s.Agent, s.ID)
	if err != nil || !ok {
		return Delivery{}, sessionEnded(s.ID)
	}
	ctx, cancel := context.WithTimeout(outer, serviceTimeout)
	defer cancel()
	c, err := inboxAt(ctx, s, rec)
	if err != nil {
		return Delivery{}, err
	}
	defer c.Close()

	d, undo, err := recordDelivery(ctx, log, s, runningGate, driver.Inbox, rec.Inbox, func() error {
		return sessionEnded(s.ID)
	}, func(rec *session.Record) eventlog.Event {
		rec.Halted = time.Now().UTC().Truncate(time.Second)
		ev := eventlog.Event{Event: eventlog.Halt, Cwd: rec.Cwd}
		caller.stamp(&ev)
		return ev
	}, nil)
	if err != nil {
		return Delivery{}, err
	}
	ctx, cancelDelivery := context.WithTimeout(outer, serviceTimeout)
	defer cancelDelivery()
	if err := c.Interrupt(ctx, s.ID); err != nil {
		return undone(outer, d, inboxFailure(d, err), undo)
	}
	return d, nil
}

// runningGate admits a session whose main agent runs a turn or waits on a
// prompt, and which tmag has not just interrupted.
func runningGate(s Session, v View, now time.Time) error {
	switch {
	case s.State == adapter.Blocked:
	case s.State == adapter.Working && s.Kind != adapter.KindBackground:
	default:
		detail := fmt.Sprintf("%s is %s; no turn to interrupt", s.ID, describe(s.State, s.Kind))
		if s.Kind == adapter.KindBackground {
			detail = fmt.Sprintf("%s runs only background work; its main agent has no turn to interrupt", s.ID)
		}
		return &RefusedError{RefuseIdle, detail}
	}
	for _, r := range v.records {
		if r.Agent == s.Agent && r.ID == s.ID && halting(r, now) {
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s was interrupted %s ago and has not reported since", s.ID, now.Sub(r.Halted).Round(time.Second))}
		}
	}
	return nil
}
