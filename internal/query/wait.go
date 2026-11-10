package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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
			if !started && (rec.Sent.IsZero() || rec.Turn != *o.Prompted) {
				started = true
			}
			if started && slices.Contains(o.States, rec.State()) {
				return rec, nil
			}
			if !started && !time.Now().Before(grace) {
				return rec, ErrNoTurn
			}
			if now := time.Now(); !now.Before(nextLive) {
				nextLive = now.Add(livenessInterval)
				if rec.Host.PID > 0 && !proc.RunningContext(ctx, rec.Host.PID, rec.Host.Start) {
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
