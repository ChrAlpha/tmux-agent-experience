package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// ErrNoTurn reports that a submitted prompt started no turn within the grace
// period, e.g. because it was a slash command.
var ErrNoTurn = errors.New("no turn started")

// livenessInterval is how often Wait checks that the session still exists.
const livenessInterval = 2 * time.Second

// WaitOptions tune Wait.
type WaitOptions struct {
	States []string
	After  int64
	// Cursor anchors an in-process observation or delivery against recreation.
	// An unset cursor preserves the numeric v1 After interface.
	Cursor eventlog.Cursor
	// StartGrace, when positive, waits for a turn that starts after o.After:
	// states logged before the session starts working or blocks do not count,
	// and Wait fails with ErrNoTurn if no turn starts within StartGrace.
	StartGrace time.Duration
}

// Wait blocks until an event of session s with seq > o.After reaches one of
// o.States, and returns it. It fails with ErrEnded when the session ends,
// wherever it runs; a pane that stops showing it does not end the wait.
func Wait(ctx context.Context, log *eventlog.Log, s Session, o WaitOptions) (eventlog.Event, error) {
	cursor := o.Cursor
	if !cursor.IsSet() {
		cursor = eventlog.Cursor{Seq: o.After}
	} else if cursor.Seq != o.After {
		return eventlog.Event{}, errors.New("wait cursor differs from After")
	}
	want := map[string]bool{}
	for _, state := range o.States {
		want[state] = true
	}
	started := o.StartGrace <= 0
	graceAt := time.Now().Add(o.StartGrace)
	var grace <-chan time.Time
	if !started {
		timer := time.NewTimer(o.StartGrace)
		defer timer.Stop()
		grace = timer.C
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	nextLive := time.Now().Add(livenessInterval)
	store := session.Store{Dir: log.Dir}
	for {
		// A grace expiry is only evidence after a certified replay captured at
		// or after that deadline. Timer cancellation cannot hide history loss.
		replayStarted := time.Now()
		batch, err := log.Replay(ctx, cursor)
		if err != nil {
			return eventlog.Event{}, err
		}
		if err := ctx.Err(); err != nil {
			return eventlog.Event{}, err
		}
		for _, e := range batch.Events {
			if e.Session != s.ID || e.Agent != s.Agent {
				continue
			}
			if e.Ended() {
				return eventlog.Event{}, ErrEnded
			}
			if e.State == "" {
				continue
			}
			if !started {
				if e.State != adapter.Working && e.State != adapter.Blocked {
					continue
				}
				started, grace = true, nil
			}
			if want[e.State] {
				return e, nil
			}
		}
		cursor = batch.Cursor
		if !started && !replayStarted.Before(graceAt) {
			return eventlog.Event{}, ErrNoTurn
		}
		now := time.Now()
		if !now.Before(nextLive) {
			nextLive = now.Add(livenessInterval)
			rec, ok, err := store.GetContext(ctx, s.Agent, s.ID)
			if err == nil {
				if !ok || rec.Ended != "" {
					return eventlog.Event{}, fmt.Errorf("%w: %s", ErrEnded, s.ID)
				}
				if rec.Host.PID > 0 && !proc.RunningContext(ctx, rec.Host.PID, rec.Host.Start) {
					return eventlog.Event{}, fmt.Errorf("%w: the process of %s exited", ErrEnded, s.ID)
				}
			}
		}
		if !started && !time.Now().Before(graceAt) {
			continue
		}
		select {
		case <-ctx.Done():
			return eventlog.Event{}, ctx.Err()
		case <-tick.C:
		case <-grace:
			grace = nil
		}
	}
}
