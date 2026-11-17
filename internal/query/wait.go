package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// ErrNoTurn reports that a submitted prompt started no turn within the grace
// period, e.g. because it was a slash command.
var ErrNoTurn = errors.New("no turn started")

// waitPoll is how often Wait reads the session's record, and livenessInterval
// how often it checks that the session's process still runs.
const (
	waitPoll         = 200 * time.Millisecond
	livenessInterval = 2 * time.Second
)

// WaitOptions tune Wait.
type WaitOptions struct {
	States []string
	// Prompted, when set, waits for the turn of a prompt tmag delivered to
	// the session while it was in this turn ("" for none): only a turn that
	// prompt started counts, and Wait fails with ErrNoTurn if none starts
	// within Grace.
	Prompted *string
	// Turn, when the prompt's channel named the turn it started, is that
	// turn: only it counts, whatever other turn starts meanwhile.
	Turn string
	// Grace is how long a prompt may take to start a turn; PendingWindow
	// when 0.
	Grace time.Duration
}

// Wait blocks until session s is in one of o.States, as its record says,
// and returns its record then. It fails with ErrEnded when the session ends,
// wherever it runs; a pane that stops showing it does not end the wait.
//
// It reads the record, where every change lands first, rather than the
// event log, so it misses no state the session stays in. A turn a prompt
// started shows there once its hooks report it: they clear the prompt's
// Sent mark, or report another turn.
func Wait(ctx context.Context, store session.Store, s Session, o WaitOptions) (session.Record, error) {
	started := o.Prompted == nil
	if o.Grace == 0 {
		o.Grace = PendingWindow
	}
	grace := time.Now().Add(o.Grace)
	nextLive := time.Now().Add(livenessInterval)
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		rec, ok, err := store.Get(ctx, s.Agent, s.ID)
		if err != nil && ctx.Err() != nil {
			return session.Record{}, ctx.Err()
		}
		if err == nil && !ok {
			return session.Record{}, fmt.Errorf("%w: %s", ErrEnded, s.ID)
		}
		if err == nil {
			switch {
			case started:
			case o.Turn != "":
				started = rec.Turn == o.Turn
			default:
				started = rec.Sent.IsZero() || rec.Turn != *o.Prompted
			}
			if started && slices.Contains(o.States, rec.State()) {
				return rec, nil
			}
			if !started && !time.Now().Before(grace) {
				return rec, ErrNoTurn
			}
			if now := time.Now(); !now.Before(nextLive) {
				nextLive = now.Add(livenessInterval)
				if rec.Host.PID > 0 && !proc.Running(ctx, rec.Host.PID, rec.Host.Start) {
					return session.Record{}, fmt.Errorf("%w: the process of %s exited", ErrEnded, s.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return session.Record{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// shownPoll is how often WaitShown looks at the pane: it reads the world,
// which costs more than a record.
const shownPoll = 500 * time.Millisecond

// WaitShown blocks until the pane shows a session tmag can prove, and
// returns that session. An agent started in a pane a moment ago, by a person
// or by another agent with tmux, shows its session from its first hook. It
// fails with ErrNotFound once the pane is gone.
func WaitShown(ctx context.Context, store session.Store, ref eventlog.PaneRef) (Session, error) {
	tick := time.NewTicker(shownPoll)
	defer tick.Stop()
	for {
		v, w, err := Load(ctx, store)
		if err == nil {
			if _, ok := w.Find(ref.Server, ref.ID); !ok {
				return Session{}, fmt.Errorf("%w: pane %s is gone", ErrNotFound, ref.ID)
			}
			if t, err := paneTarget(v, ref, ref.ID); err == nil && t.Session != nil {
				return *t.Session, nil
			}
		}
		select {
		case <-ctx.Done():
			return Session{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// Reached is where one session of a wait over several stands: the state it
// reached, whether it settled the asks that were waited on, or whether it
// ended.
type Reached struct {
	Session Session `json:"session"`
	State   string  `json:"state,omitempty"`
	Kind    string  `json:"kind,omitempty"`
	// Settled says it owes the caller nothing more of what was waited on:
	// it replied, or the turn a final ask started ended.
	Settled bool `json:"settled,omitempty"`
	Ended   bool `json:"ended,omitempty"`
}

// Awaited is one session a wait over several waits on: for one of states,
// or, when Owed names obligations, until it owes none of them or needs the
// person.
type Awaited struct {
	Session Session
	States  []string
	Owed    []string
}

// Owing are the sessions that owe the caller an answer, each with every
// obligation it owes the caller: what wait --asked waits on.
func Owing(v View, c Caller) ([]Awaited, error) {
	if c.By == nil || c.By.Session == "" {
		return nil, ErrNoSender
	}
	me, now := asker(c), time.Now()
	var out []Awaited
	for _, r := range v.records {
		var ids []string
		for _, o := range r.Owing(now) {
			if o.To == me {
				ids = append(ids, o.ID)
			}
		}
		if s, ok := v.Session(r.Agent, r.ID); ok && ids != nil {
			out = append(out, Awaited{Session: s, Owed: ids})
		}
	}
	return out, nil
}

// WaitMany blocks until any of the awaited sessions, or all of them, reach
// what each waits for, and returns where each one that did stands. An ended
// session counts as reached. It reads records, as Wait does.
func WaitMany(ctx context.Context, store session.Store, awaited []Awaited, all bool) ([]Reached, error) {
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	nextLive := time.Now()
	for {
		live := !time.Now().Before(nextLive)
		if live {
			nextLive = time.Now().Add(livenessInterval)
		}
		var reached []Reached
		for _, a := range awaited {
			if r, ok := standing(ctx, store, a, live); ok {
				reached = append(reached, r)
			}
		}
		if err := ctx.Err(); err != nil {
			return reached, err
		}
		if len(reached) > 0 && (!all || len(reached) == len(awaited)) {
			return reached, nil
		}
		select {
		case <-ctx.Done():
			return reached, ctx.Err()
		case <-tick.C:
		}
	}
}

// standing reads where one awaited session stands, and whether that is what it
// is waited for. live checks that its process still runs.
func standing(ctx context.Context, store session.Store, a Awaited, live bool) (Reached, bool) {
	s := a.Session
	rec, ok, err := store.Get(ctx, s.Agent, s.ID)
	switch {
	case err != nil:
		return Reached{}, false
	case !ok, live && rec.Host.PID > 0 && !proc.Running(ctx, rec.Host.PID, rec.Host.Start):
		return Reached{Session: s, Ended: true}, true
	}
	r := Reached{Session: s, State: rec.State(), Kind: rec.Kind()}
	if a.Owed == nil {
		return r, slices.Contains(a.States, r.State)
	}
	r.Settled = !slices.ContainsFunc(rec.Owing(time.Now()), func(o eventlog.Obligation) bool { return slices.Contains(a.Owed, o.ID) })
	return r, r.Settled || r.State == adapter.Blocked
}
