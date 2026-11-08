package eventlog

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrHistoryUnavailable means continuation cannot be certified. A state mirror
// must replace its image from a snapshot; a historical predicate may be unknown.
var ErrHistoryUnavailable = errors.New("event history unavailable")

// ReadTimeout bounds each capture, rather than a stream's whole lifetime.
// A canceled parent or an earlier deadline always takes precedence.
const ReadTimeout = 5 * time.Second

// HistoryUnavailable describes why a cursor cannot be continued. It carries no
// events: later events must not be mistaken for a complete continuation.
type HistoryUnavailable struct {
	After  int64
	Reason string
}

func (e *HistoryUnavailable) Error() string {
	return fmt.Sprintf("%s after %d: %s", ErrHistoryUnavailable, e.After, e.Reason)
}

func (e *HistoryUnavailable) Unwrap() error { return ErrHistoryUnavailable }

// Cursor is an in-process checkpoint. Seq retains the v1 allocation cursor;
// the private identity detects recreation or repair even if numbers overlap.
// Its file position only accelerates reads and never establishes continuity.
type Cursor struct {
	Seq        int64
	epoch      string
	generation uint64
	current    segment
}

// IsSet reports whether this cursor carries an in-process log identity.
func (c Cursor) IsSet() bool { return c.epoch != "" }

func (h history) cursor() Cursor {
	return Cursor{Seq: h.Allocated, epoch: h.Epoch, generation: h.Generation, current: h.Current}
}

// Head captures a cursor under the writer lock, after recovery. A reservation
// without a record is included: no future writer may fill that unused number.
func (l *Log) Head(ctx context.Context) (Cursor, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	var c Cursor
	err := l.WithLock(ctx, func() error {
		h, err := l.history(ctx)
		if err == nil {
			c = h.cursor()
		}
		return err
	})
	return c, err
}

// Batch is an immutable replay capture through Cursor. Callbacks and writes to
// clients take place after capture releases the log lock.
type Batch struct {
	Events []Event
	Cursor Cursor
}

func (h history) continuation(c Cursor) error {
	reason := ""
	switch {
	case c.epoch != "" && c.epoch != h.Epoch:
		reason = "log recreated"
	case c.epoch != "" && c.generation != h.Generation:
		reason = "log repaired or replaced"
	case c.Seq > h.Allocated:
		reason = "cursor ahead of log"
	case c.Seq < h.Floor:
		reason = "records discarded or continuity invalidated"
	case c.Seq != 0 && c.Seq < h.Origin:
		reason = "cursor predates log"
	case c.Seq < 0:
		reason = "negative cursor"
	}
	if reason != "" {
		return &HistoryUnavailable{After: c.Seq, Reason: reason}
	}
	return nil
}

// Replay returns only a certified continuation. Numeric v1 cursors use the
// persisted origin/retention/invalidation fences, without requiring new fields.
func (l *Log) Replay(ctx context.Context, after Cursor) (Batch, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	var batch Batch
	err := l.WithLock(ctx, func() error {
		h, err := l.history(ctx)
		if err != nil {
			return err
		}
		if err := h.continuation(after); err != nil {
			return err
		}
		batch.Events, err = l.events(ctx, h, after)
		batch.Cursor = h.cursor()
		return err
	})
	if err != nil {
		return Batch{}, err
	}
	return batch, nil
}

func (l *Log) events(ctx context.Context, h history, after Cursor) ([]Event, error) {
	var events []Event
	for i, s := range []segment{h.Rotated, h.Current} {
		if s.Last <= after.Seq {
			continue
		}
		name := rotatedName
		if i == 1 {
			name = currentName
		}
		start, previous := int64(0), int64(0)
		if after.epoch == h.Epoch && after.generation == h.Generation &&
			after.current.sameFile(s) && after.current.Size <= s.Size && after.current.Last <= after.Seq {
			start, previous = after.current.Size, after.current.Last
		}
		_, more, partial, err := l.scan(ctx, name, start, previous, after.Seq, true)
		if err != nil {
			return nil, err
		}
		if partial {
			return nil, &HistoryUnavailable{After: after.Seq, Reason: "unfinished record"}
		}
		events = append(events, more...)
	}
	return events, nil
}

// Since preserves the v1 retained-history inspection API. complete=false means
// events are only a retained suffix, not a valid continuation. Follow/Replay
// return ErrHistoryUnavailable instead of ever delivering that suffix as whole.
func (l *Log) Since(ctx context.Context, after int64) (events []Event, complete bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	err = l.WithLock(ctx, func() error {
		h, err := l.history(ctx)
		if err != nil {
			return err
		}
		complete = h.continuation(Cursor{Seq: after}) == nil
		events, err = l.events(ctx, h, Cursor{Seq: after})
		return err
	})
	return events, complete, err
}

// Follow follows a v1 numeric cursor. After initial capture, it carries the
// stronger in-process identity through every rotation/recovery check.
func (l *Log) Follow(ctx context.Context, after int64, interval time.Duration, fn func(Event) error) error {
	return l.FollowCursor(ctx, Cursor{Seq: after}, interval, fn)
}

// FollowCursor delivers verified batches in order, until canceled or fn fails.
// Its position never retains a partial record or an open stale inode. A slow
// callback can outlive retained history; the next capture then fails explicitly.
func (l *Log) FollowCursor(ctx context.Context, after Cursor, interval time.Duration, fn func(Event) error) error {
	return l.FollowBatches(ctx, after, interval, func(batch Batch) error {
		for _, e := range batch.Events {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		return nil
	})
}

// FollowBatches also calls fn for an empty certified batch. A stream can perform
// its periodic reconciliation in that same output loop, with no stale pane
// writer surviving a snapshot replacement. fn always runs outside the log lock.
func (l *Log) FollowBatches(ctx context.Context, after Cursor, interval time.Duration, fn func(Batch) error) error {
	if interval <= 0 {
		return fmt.Errorf("follow interval must be positive")
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		batch, err := l.Replay(ctx, after)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(batch); err != nil {
			return err
		}
		after = batch.Cursor
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
