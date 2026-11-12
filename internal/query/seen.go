package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Seen marks a turn of a session seen by the person, at the terminal or by
// command. turn defaults to the session's current turn when empty. The
// session must be idle on that turn; a turn already seen succeeds without an
// event, and an agent never marks one.
//
// The decision is made again under the session's lock, and the Seen event is
// logged before the lock is released.
func Seen(ctx context.Context, log *eventlog.Log, target, turn string) (Delivery, error) {
	v, caller, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	if err := seenGate(caller); err != nil {
		return Delivery{}, err
	}
	s, _, err := controlled(ctx, v, target)
	if err != nil {
		return Delivery{}, err
	}
	if turn == "" {
		turn = s.Turn
	}
	if len(turn) > session.MaxTurn {
		return Delivery{}, fmt.Errorf("turn id is longer than %d bytes", session.MaxTurn)
	}
	if turn == "" {
		return Delivery{}, &RefusedError{RefuseIdle, fmt.Sprintf("%s is idle on no turn", s.ID)}
	}
	_, seq, _, err := markSeen(ctx, log, s.Agent, s.ID, turn, caller)
	if err != nil {
		return Delivery{}, err
	}
	s.Seen = turn
	return Delivery{Session: s, Seq: seq}, nil
}

// seenGate refuses an agent: a turn seen on an agent's word would hide work
// the person has not read. There is no option to allow it.
func seenGate(c Caller) error {
	if c.By == nil {
		return nil
	}
	return &RefusedError{RefuseAgent, fmt.Sprintf("a %s agent runs this command, and only the person marks a turn seen", c.By.Agent)}
}

// MarkShown marks seen every finished turn whose session the terminal shows,
// when a watched pane shows it, a client of its server had the person's input
// within notify.Present, and @tmag-seen is not off there. server limits it to
// one tmux server when not empty. It returns the sessions it marked, with
// seen set. An unreadable record or a session that ended is left out; the next
// hook writes it anew.
//
// A view is only a list of candidates: the record is read again under its
// lock, and the watched panes and the server's presence are observed after
// that read, so a view from before a turn ended cannot mark it.
func MarkShown(ctx context.Context, log *eventlog.Log, v View, w *attach.World, server string) ([]Delivery, error) {
	caller := v.Caller(w)
	if err := seenGate(caller); err != nil {
		return nil, err
	}
	scope := ""
	if server != "" {
		scope = tmux.ServerByName(server).Socket
	}
	var out []Delivery
	var firstErr error
	for _, s := range v.Sessions {
		if !s.Finished() {
			continue
		}
		rec, seq, changed, err := markShown(ctx, log, s, scope, caller)
		if err != nil {
			var refusal *RefusedError
			var record *session.RecordError
			if errors.As(err, &refusal) || errors.As(err, &record) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !changed {
			continue
		}
		s.Seen = rec.Seen
		out = append(out, Delivery{Session: s, Seq: seq})
	}
	return out, firstErr
}

// markShown reads the session's record under its lock and, only when it is
// still finished there, asks what the terminal shows now. It writes seen and
// logs the Seen event before releasing the lock.
func markShown(ctx context.Context, log *eventlog.Log, s Session, scope string, caller Caller) (session.Record, int64, bool, error) {
	store := session.Store{Dir: log.Dir}
	var rec session.Record
	var seq int64
	var marked bool
	err := store.Lock(ctx, s.Agent, s.ID, func() error {
		cur, ok, err := store.Get(ctx, s.Agent, s.ID)
		if err != nil {
			return err
		}
		if !ok || !cur.Finished() || len(s.Panes) == 0 {
			return nil
		}
		view, world, err := Load(ctx, store)
		if err != nil {
			return err
		}
		target, ok := view.Session(cur.Agent, cur.ID)
		if !ok || target.State != cur.State() || target.Turn != cur.Turn || target.Seen != cur.Seen || len(target.Panes) == 0 {
			return nil
		}
		if !shownPanes(ctx, target.Panes, world, scope) {
			return nil
		}
		next, ok := session.MarkSeen(cur, cur.Turn)
		if !ok {
			return nil
		}
		ev := eventlog.Event{Event: eventlog.Seen, Agent: cur.Agent, Session: cur.ID, Turn: cur.Turn}
		caller.stamp(&ev)
		if err := store.Put(ctx, next); err != nil {
			return err
		}
		if err := log.Append(ctx, &ev); err != nil {
			return fmt.Errorf("%s wrote but not logged; the session's next event reports it: %w", eventlog.Seen, err)
		}
		rec, seq, marked = next, ev.Seq, true
		return nil
	})
	return rec, seq, marked, err
}

// shownPanes reports whether the terminal shows the session on a watched
// pane now. The view's panes are the proof a fresh Load built from all live
// sessions, exactly as list and jump prove them; the world says which are
// watched. Presence is read after that proof, once per pane server.
func shownPanes(ctx context.Context, panes []Shown, w *attach.World, scope string) bool {
	for _, sh := range panes {
		if scope != "" && sh.Server != scope {
			continue
		}
		p, ok := w.Find(sh.Server, sh.ID)
		if !ok || !p.Watched {
			continue
		}
		srv := tmux.Server{Socket: sh.Server}
		last, ok := srv.LastInput(ctx)
		if !ok || time.Since(last) >= notify.Present {
			continue
		}
		if srv.GlobalOption(ctx, tmux.OptionSeen) == "off" {
			continue
		}
		return true
	}
	return false
}

// markSeen takes the session's lock, decides again and writes seen, then
// logs the Seen event before releasing the lock. It reports whether the
// record changed: a turn already seen is a quiet success.
func markSeen(ctx context.Context, log *eventlog.Log, agent, id, turn string, caller Caller) (session.Record, int64, bool, error) {
	store := session.Store{Dir: log.Dir}
	var rec session.Record
	var seq int64
	var marked bool
	err := store.Lock(ctx, agent, id, func() error {
		cur, ok, err := store.Get(ctx, agent, id)
		if err != nil {
			return err
		}
		if !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", id)}
		}
		switch {
		case cur.State() != adapter.Idle:
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s; a turn is seen when the session is idle on it", id, describe(cur.State(), cur.Kind()))}
		case cur.Turn != turn:
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s is idle on turn %s, not %s", id, cur.Turn, turn)}
		}
		if cur.Seen == turn {
			rec = cur
			return nil
		}
		next, ok := session.MarkSeen(cur, turn)
		if !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has not finished turn %s", id, turn)}
		}
		ev := eventlog.Event{Event: eventlog.Seen, Agent: agent, Session: id, Turn: turn}
		caller.stamp(&ev)
		if err := store.Put(ctx, next); err != nil {
			return err
		}
		if err := log.Append(ctx, &ev); err != nil {
			return fmt.Errorf("%s wrote but not logged; the session's next event reports it: %w", eventlog.Seen, err)
		}
		rec, seq, marked = next, ev.Seq, true
		return nil
	})
	return rec, seq, marked, err
}
