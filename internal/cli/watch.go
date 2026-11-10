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
	watchRescan = 2 * time.Second
	watchPoll   = 200 * time.Millisecond
)

func (a *app) cmdWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	since := fs.Int64("since", -1, "")
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
			if err := attach.Reconcile(ctx, attach.Env{Log: a.log, Store: a.store()}, w); err != nil {
				return nil, err
			}
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
	err = s.run(ctx, *since)
	return a.watchEnd(err)
}

// watchStream owns every output in one loop, so that a snapshot, the events
// after it and pane scans never race each other.
type watchStream struct {
	log     *eventlog.Log
	capture func(context.Context) (query.Snapshot, error)
	rescan  func(context.Context) ([]query.Pane, error)
	emit    func(map[string]any) error
	shown   string
}

func (s *watchStream) events(ctx context.Context, events []eventlog.Event) error {
	for _, e := range events {
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

// snapshot sends every session and pane as they are, after the event its
// seq names, and returns a tail of the events after that one. The tail is
// placed first: what changes while the view is read is then both in the view
// and among the events, and an event applied twice changes nothing.
func (s *watchStream) snapshot(ctx context.Context) (*eventlog.Tail, error) {
	tail, err := s.log.Follow(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := s.capture(ctx)
	if err == nil {
		err = s.emit(map[string]any{"type": "snapshot", "seq": tail.Seq(), "sessions": snap.Sessions, "panes": snap.Panes})
	}
	if err != nil {
		tail.Close()
		return nil, err
	}
	b, _ := json.Marshal(snap.Panes)
	s.shown = string(b)
	return tail, nil
}

// run sends a snapshot, unless since names an event the log still goes on
// from, then the events that follow, and the panes whenever they change.
// Whenever the log cannot show it sent every event since, because some
// rotated away or the log was recreated, it sends a new snapshot and goes on
// from there.
func (s *watchStream) run(ctx context.Context, since int64) error {
	var tail *eventlog.Tail
	defer func() {
		if tail != nil {
			tail.Close()
		}
	}()
	if since >= 0 {
		tail = s.log.Tail(since)
		events, complete, err := tail.Next(ctx)
		if err != nil {
			return err
		}
		if !complete {
			tail.Close()
			tail = nil
		} else if err := s.events(ctx, events); err != nil {
			return err
		} else {
			list, err := s.rescan(ctx)
			if err != nil {
				return err
			}
			if err := s.panes(ctx, list); err != nil {
				return err
			}
		}
	}
	if tail == nil {
		var err error
		if tail, err = s.snapshot(ctx); err != nil {
			return err
		}
	}
	nextScan := time.Now().Add(watchRescan)
	tick := time.NewTicker(watchPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		events, complete, err := tail.Next(ctx)
		if err != nil {
			return err
		}
		if !complete {
			tail.Close()
			if tail, err = s.snapshot(ctx); err != nil {
				return err
			}
			nextScan = time.Now().Add(watchRescan)
			continue
		}
		if err := s.events(ctx, events); err != nil {
			return err
		}
		if now := time.Now(); !now.Before(nextScan) {
			nextScan = now.Add(watchRescan)
			list, err := s.rescan(ctx)
			if err == nil {
				if err := s.panes(ctx, list); err != nil {
					return err
				}
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		}
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
