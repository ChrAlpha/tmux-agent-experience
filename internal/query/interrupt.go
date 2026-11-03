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
// session's driver uses, and returns where it went.
//
// The session must be working on a turn of its main agent or blocked on a
// prompt (which an interrupt declines), and not interrupted by tmag since
// the agent last reported. The Halt event is logged under the session's lock.
// What the agent does next comes from its hooks: Codex reports the interrupt,
// Claude Code reports nothing until it notices it is idle.
func Interrupt(ctx context.Context, log *eventlog.Log, target string, from *eventlog.PaneRef) (Delivery, error) {
	v, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	s, named, err := controlled(v, target)
	if err != nil {
		return Delivery{}, err
	}
	switch driver.Channel(s.Agent, s.Runtime, driver.Interrupt) {
	case driver.Service:
		var turn string
		return throughService(ctx, log, v, s, serviceVerb{
			gate: runningGate,
			check: func(ctx context.Context, c *codexsvc.Client) error {
				return threadIn(ctx, c, s, RefuseIdle, codexsvc.StatusActive)
			},
			mark: func(rec *session.Record) eventlog.Event {
				rec.Halted, turn = time.Now().UTC().Truncate(time.Second), rec.Turn
				return eventlog.Event{Event: eventlog.Halt, Cwd: rec.Cwd, From: from}
			},
			deliver: func(ctx context.Context, c *codexsvc.Client) error {
				return c.InterruptTurn(ctx, s.ID, turn)
			},
		})
	case driver.Pane:
		store := session.Store{Dir: log.Dir}
		return throughPane(ctx, log, v, s, named, runningGate, func(ctx context.Context, s Session, p Pane, rec session.Record) (eventlog.Event, error) {
			rec.Halted = time.Now().UTC().Truncate(time.Second)
			if err := store.Put(rec); err != nil {
				return eventlog.Event{}, err
			}
			if err := (tmux.Server{Socket: p.Server}).Interrupt(ctx, p.ID); err != nil {
				return eventlog.Event{}, fmt.Errorf("pressing Escape in %s failed: %w", p.ID, err)
			}
			return eventlog.Event{Event: eventlog.Halt, Cwd: p.Cwd, From: from}, nil
		})
	}
	return Delivery{}, unsupported(s, driver.Interrupt)
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
