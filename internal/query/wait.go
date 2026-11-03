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
	// StartGrace, when positive, waits for a turn that starts after o.After:
	// states logged before the session starts working or blocks do not count,
	// and Wait fails with ErrNoTurn if no turn starts within StartGrace.
	StartGrace time.Duration
}

// Wait blocks until an event of session s with seq > o.After reaches one of
// o.States, and returns it. It fails with ErrEnded when the session ends,
// wherever it runs; a pane that stops showing it does not end the wait.
func Wait(ctx context.Context, log *eventlog.Log, s Session, o WaitOptions) (eventlog.Event, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	want := map[string]bool{}
	for _, st := range o.States {
		want[st] = true
	}
	seen := make(chan struct{})
	go watchLiveness(ctx, cancel, session.Store{Dir: log.Dir}, s, seen, o.StartGrace)

	var found eventlog.Event
	errFound := errors.New("found")
	started := o.StartGrace <= 0
	err := log.Follow(ctx, o.After, 200*time.Millisecond, func(e eventlog.Event) error {
		if e.Session != s.ID || e.Agent != s.Agent {
			return nil
		}
		if e.Ended() {
			return ErrEnded
		}
		if e.State == "" {
			return nil // Send, Attach, Detach
		}
		if !started {
			if e.State != adapter.Working && e.State != adapter.Blocked {
				return nil
			}
			started = true
			close(seen)
		}
		if want[e.State] {
			found = e
			return errFound
		}
		return nil
	})
	switch {
	case errors.Is(err, errFound):
		return found, nil
	case context.Cause(ctx) != nil && !errors.Is(context.Cause(ctx), context.Canceled):
		return eventlog.Event{}, context.Cause(ctx)
	}
	return eventlog.Event{}, err
}

func watchLiveness(ctx context.Context, cancel context.CancelCauseFunc, store session.Store, s Session, seen <-chan struct{}, grace time.Duration) {
	var graceC <-chan time.Time
	if grace > 0 {
		graceC = time.After(grace)
	}
	tick := time.NewTicker(livenessInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-seen:
			seen, graceC = nil, nil
		case <-graceC:
			cancel(ErrNoTurn)
			return
		case <-tick.C:
			rec, ok, err := store.Get(s.Agent, s.ID)
			if err != nil {
				continue
			}
			if !ok || rec.Ended != "" {
				cancel(fmt.Errorf("%w: %s", ErrEnded, s.ID))
				return
			}
			if rec.Host.PID > 0 {
				if !proc.Running(rec.Host.PID, rec.Host.Start) {
					cancel(fmt.Errorf("%w: the process of %s exited", ErrEnded, s.ID))
					return
				}
			}
		}
	}
}
