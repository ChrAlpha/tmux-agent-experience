package query

import (
	"context"
	"errors"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/observe"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

const snapshotAttempts = 3

func takeSnapshot(ctx context.Context, log *eventlog.Log, load func(context.Context) (View, *attach.World, error)) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, eventlog.ReadTimeout)
	defer cancel()
	for range snapshotAttempts {
		start, err := log.Head(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		v, w, err := load(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		backlog, err := log.Replay(ctx, start)
		if errors.Is(err, eventlog.ErrHistoryUnavailable) {
			continue
		}
		if err != nil {
			return Snapshot{}, err
		}
		caller := callerFor(ctx, v, w)
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		return Snapshot{Seq: start.Seq, Caller: caller, View: v, Backlog: backlog}, nil
	}
	return Snapshot{}, &eventlog.HistoryUnavailable{Reason: "snapshot repeatedly outpaced retained history"}
}

// TakeObservedSnapshot emits no cached caller. Its image and copied replay
// overlap share the pre-scan full log checkpoint, never a new head.
func TakeObservedSnapshot(ctx context.Context, log *eventlog.Log) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, eventlog.ReadTimeout)
	defer cancel()
	mode := observe.Bootstrap
	env := attach.Env{Log: log, Store: session.Store{Dir: log.Dir}}
	for range snapshotAttempts {
		img, _, err := observe.Refresh(ctx, env, mode)
		if errors.Is(err, eventlog.ErrHistoryUnavailable) {
			mode = observe.Retry
			continue
		}
		if err != nil {
			return Snapshot{}, err
		}
		start, err := img.Start.Cursor()
		if err != nil {
			return Snapshot{}, err
		}
		backlog, err := log.Replay(ctx, start)
		if errors.Is(err, eventlog.ErrHistoryUnavailable) {
			mode = observe.Retry
			continue
		}
		if err != nil {
			return Snapshot{}, err
		}
		return Snapshot{Seq: start.Seq, View: buildRecords(img.Records, img.Panes), Backlog: backlog}, nil
	}
	return Snapshot{}, &eventlog.HistoryUnavailable{Reason: "shared image repeatedly outpaced retained history"}
}
