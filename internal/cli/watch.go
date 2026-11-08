package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

const (
	watchRescan      = 2 * time.Second
	watchPoll        = 200 * time.Millisecond
	watchResetWindow = 30 * time.Second
	watchResetLimit  = 3
)

func (a *app) cmdWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	since := fs.Int64("since", -1, "")
	resync := fs.Bool("resync", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err != nil {
		return a.usageError("watch", err)
	}
	a.json = true
	ctx, cancel := a.signalContext()
	defer cancel()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	if f, ok := a.stdout.(*os.File); ok {
		if gone := hangup(f, ctx.Done()); gone != nil {
			go func() {
				select {
				case <-gone:
					stop(errReaderGone)
				case <-ctx.Done():
				}
			}()
		}
	}
	writer := a.stdout
	if f, ok := writer.(*os.File); ok {
		owned, err := contextio.Own(ctx, f)
		if err != nil {
			return a.watchEnd(err)
		}
		defer owned.Close()
		writer = owned
		// Terminal error frames use app.emit too; they share the same owned
		// boundary instead of falling back to the inherited blocking stream.
		original := a.stdout
		a.stdout = owned
		defer func() { a.stdout = original }()
	}
	out := bufio.NewWriter(writer)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	s := watchStream{
		log: a.log,
		capture: func(ctx context.Context) (query.Snapshot, error) {
			return query.TakeSnapshot(ctx, a.log)
		},
		rescan: func(ctx context.Context) ([]query.Pane, error) {
			ctx, cancel := context.WithTimeout(ctx, eventlog.ReadTimeout)
			defer cancel()
			w, err := attach.Load(ctx)
			if err != nil {
				return nil, err
			}
			attach.Reconcile(ctx, attach.Env{Log: a.log, Store: a.store()}, w)
			v, _, err := query.Load(ctx, a.store())
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return v.Panes, err
		},
		emit: func(doc map[string]any) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			doc["schema"] = version.Schema
			if err := enc.Encode(doc); err != nil {
				return err
			}
			return out.Flush()
		},
	}
	err = s.run(ctx, *since, *resync)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	return a.watchEnd(err)
}

// watchStream owns every output in one loop. Snapshot replacement, copied
// backlog, subsequent events and pane scans cannot race another output writer.
type watchStream struct {
	log     *eventlog.Log
	capture func(context.Context) (query.Snapshot, error)
	rescan  func(context.Context) ([]query.Pane, error)
	emit    func(map[string]any) error
	shown   string
}

func (s *watchStream) events(ctx context.Context, batch eventlog.Batch) error {
	for _, e := range batch.Events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.emit(map[string]any{"type": "event", "event": e}); err != nil {
			return err
		}
	}
	return nil
}

func (s *watchStream) panes(ctx context.Context, list []query.Pane) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if string(b) == s.shown {
		return nil
	}
	s.shown = string(b)
	return s.emit(map[string]any{"type": "panes", "panes": list})
}

func (s *watchStream) snapshot(ctx context.Context) (eventlog.Cursor, error) {
	snap, err := s.capture(ctx)
	if err != nil {
		return eventlog.Cursor{}, err
	}
	if err := ctx.Err(); err != nil {
		return eventlog.Cursor{}, err
	}
	if err := s.emit(map[string]any{"type": "snapshot", "seq": snap.Seq, "sessions": snap.Sessions, "panes": snap.Panes}); err != nil {
		return eventlog.Cursor{}, err
	}
	b, _ := json.Marshal(snap.Panes)
	s.shown = string(b)
	// These records were copied before snapshot publication. Retention can
	// change while output blocks, but cannot invalidate the captured bytes.
	if err := s.events(ctx, snap.Backlog); err != nil {
		return eventlog.Cursor{}, err
	}
	return snap.Backlog.Cursor, nil
}

func (s *watchStream) start(ctx context.Context, since int64) (eventlog.Cursor, error) {
	if since >= 0 {
		batch, err := s.log.Replay(ctx, eventlog.Cursor{Seq: since})
		if err == nil {
			if err := s.events(ctx, batch); err != nil {
				return eventlog.Cursor{}, err
			}
			list, err := s.rescan(ctx)
			if err != nil {
				return eventlog.Cursor{}, err
			}
			return batch.Cursor, s.panes(ctx, list)
		}
		if !errors.Is(err, eventlog.ErrHistoryUnavailable) {
			return eventlog.Cursor{}, err
		}
	}
	// v1 already replaces an unavailable initial replay with a snapshot.
	return s.snapshot(ctx)
}

func (s *watchStream) run(ctx context.Context, since int64, resync bool) error {
	last, err := s.start(ctx, since)
	if err != nil {
		return err
	}
	nextScan := time.Now().Add(watchRescan)
	var resets []time.Time
	for {
		err = s.log.FollowBatches(ctx, last, watchPoll, func(batch eventlog.Batch) error {
			if err := s.events(ctx, batch); err != nil {
				return err
			}
			if now := time.Now(); !now.Before(nextScan) {
				nextScan = now.Add(watchRescan)
				list, err := s.rescan(ctx)
				if err == nil {
					return s.panes(ctx, list)
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			return nil
		})
		if !resync || !errors.Is(err, eventlog.ErrHistoryUnavailable) {
			return err
		}
		now := time.Now()
		for len(resets) > 0 && now.Sub(resets[0]) >= watchResetWindow {
			resets = resets[1:]
		}
		if len(resets) >= watchResetLimit {
			return fmt.Errorf("watch resync limit reached: %w", err)
		}
		resets = append(resets, now)
		last, err = s.snapshot(ctx)
		if err != nil {
			return err
		}
		nextScan = time.Now().Add(watchRescan)
	}
}

func (a *app) watchEnd(err error) int {
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errReaderGone) && !isClosedPipe(err) {
		return a.fail("watch", err)
	}
	return ExitOK
}

func isClosedPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}
