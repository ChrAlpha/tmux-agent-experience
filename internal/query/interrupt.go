package query

import (
	"context"
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
	var tried error
	for _, channel := range driver.InterruptPlan(s.Agent, s.Runtime).Try {
		var d Delivery
		switch channel {
		case driver.Inbox:
			d, tried = throughInboxInterrupt(ctx, log, v, s, caller)
		case driver.Service:
			d, tried = throughService(ctx, log, v, s, serviceInterrupt(s, caller))
		case driver.Pane:
			d, tried = throughPane(ctx, log, v, s, named, runningGate, paneInterrupt(caller))
		}
		if !unbound(tried) {
			return d, tried
		}
	}
	if tried != nil {
		return Delivery{}, tried
	}
	return Delivery{}, unsupported(s, driver.Interrupt)
}

// serviceInterrupt is the Codex daemon's interrupt: of the active turn the
// fresh record names.
func serviceInterrupt(s Session, caller Caller) serviceVerb {
	var turn string
	return serviceVerb{
		gate: runningGate,
		check: func(ctx context.Context, c *codexsvc.Client) error {
			return threadIn(ctx, c, s, RefuseIdle, codexsvc.StatusActive)
		},
		mark: func(rec *session.Record) eventlog.Event {
			rec.Halted, turn = time.Now().UTC(), rec.Turn
			ev := eventlog.Event{Event: eventlog.Halt, Cwd: rec.Cwd}
			caller.stamp(&ev)
			return ev
		},
		deliver: func(ctx context.Context, c *codexsvc.Client) error {
			return c.InterruptTurn(ctx, s.ID, turn)
		},
	}
}

// paneInterrupt presses Escape in the session's pane. An agent that reports
// nothing of it has its turn ended by tmag, with whatever it asked the user.
func paneInterrupt(caller Caller) paneVerb {
	return paneVerb{
		plan: func(s Session, p Pane, rec session.Record) (session.Record, eventlog.Event, *session.Record) {
			now := time.Now()
			rec.Halted = now.UTC()
			ev := eventlog.Event{Event: eventlog.Halt, Cwd: p.Cwd}
			caller.stamp(&ev)
			if driver.ReportsInterrupt(s.Agent) {
				return rec, ev, nil
			}
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
	}
}

// throughInboxInterrupt stops s's turn through the inbox its own agent
// serves. The inbox is dialled and proven before the Halt is recorded, and
// the record is marked under the session's lock against fresh state before
// the extension is asked; the extension checks the session again at delivery.
func throughInboxInterrupt(outer context.Context, log *eventlog.Log, v View, s Session, caller Caller) (Delivery, error) {
	if err := gateOf(v, s, runningGate); err != nil {
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

	d, undo, project, err := recordDelivery(ctx, log, s, runningGate, driver.Inbox, rec.Inbox, func() error {
		return sessionEnded(s.ID)
	}, func(rec *session.Record) eventlog.Event {
		rec.Halted = time.Now().UTC()
		ev := eventlog.Event{Event: eventlog.Halt, Cwd: rec.Cwd}
		caller.stamp(&ev)
		return ev
	}, nil)
	defer project(outer)
	if err != nil {
		return Delivery{}, err
	}
	if err := outer.Err(); err != nil {
		return undone(outer, d, &NotTakenError{Seq: d.Seq, Target: s.ID, Err: err}, undo) // nothing written
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
func runningGate(rec session.Record, now time.Time) error {
	state, kind := rec.State(), rec.Kind()
	switch {
	case state == adapter.Blocked:
	case state == adapter.Working && kind != adapter.KindBackground:
	default:
		detail := fmt.Sprintf("%s is %s; no turn to interrupt", rec.ID, describe(state, kind))
		if kind == adapter.KindBackground {
			detail = fmt.Sprintf("%s runs only background work; its main agent has no turn to interrupt", rec.ID)
		}
		return &RefusedError{RefuseIdle, detail}
	}
	if halting(rec, now) {
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s was interrupted %s ago and has not reported since", rec.ID, now.Sub(rec.Halted).Round(time.Second))}
	}
	return nil
}
