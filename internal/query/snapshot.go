package query

import (
	"context"
	"errors"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
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
