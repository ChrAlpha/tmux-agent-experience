package attach

import (
	"context"
	"errors"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Env is where attachment changes are recorded.
type Env struct {
	Log   *eventlog.Log
	Store session.Store
}

// Shown is a pane whose client validly shows a session.
type Shown struct {
	Client     Client
	Attachment Attachment
	View       string
}

// Attached returns the panes in w that show rec.
func (w *World) Attached(rec session.Record) []Shown {
	var out []Shown
	for _, p := range w.Panes {
		a, ok := FromPane(p)
		if !ok || !a.holds() || a.Agent != rec.Agent || a.Session != rec.ID {
			continue
		}
		if view, valid := w.Check(p, a, rec); valid {
			c, _ := w.ClientIn(p)
			out = append(out, Shown{Client: c, Attachment: a, View: view})
		}
	}
	return out
}

// Live splits sessions into those that run and those that are gone: ended,
// or whose host process exited without SessionEnd.
func Live(w *World, recs []session.Record) (live, gone []session.Record) {
	for _, r := range recs {
		if r.Ended != "" || (r.Host.PID > 0 && !w.Proc.Alive(r.Host.PID, r.Host.Start)) {
			gone = append(gone, r)
		} else {
			live = append(live, r)
		}
	}
	return live, gone
}

// Link is an agent client in a pane and the session it shows, if any, as
// the world proves it.
type Link struct {
	Client Client
	// Session is the session shown, nil when the pane shows none tmag knows.
	Session  *session.Record
	Evidence string
	View     string
	// Stored is the attachment on the pane, when it is the one Link
	// describes; false when it is missing or different.
	Stored bool
}

// Links returns one Link per agent client in w, given the live sessions:
//
//   - an attachment stored on the pane holds while Check accepts it;
//   - otherwise a Codex client whose title shows exactly one live Codex
//     session's thread shows that session, by title-id;
//   - otherwise the client shows nothing tmag can prove.
func Links(w *World, live []session.Record) []Link {
	index := make(map[[2]string]*session.Record, len(live))
	for i := range live {
		index[[2]string{live[i].Agent, live[i].ID}] = &live[i]
	}
	var out []Link
	for _, c := range w.Clients() {
		l := Link{Client: c}
		if a, ok := FromPane(c.Pane); ok && a.holds() {
			if rec := index[[2]string{a.Agent, a.Session}]; rec != nil {
				if view, valid := w.Check(c.Pane, a, *rec); valid {
					l.Session, l.Evidence, l.View, l.Stored = rec, a.Evidence, view, true
				}
			}
		}
		if l.Session == nil && c.Agent == proc.Codex {
			var claimed []*session.Record
			var views []string
			for i := range live {
				if view, shown := shows(c.Pane.Title, live[i]); shown && live[i].Agent == proc.Codex {
					claimed, views = append(claimed, &live[i]), append(views, view)
				}
			}
			if len(claimed) == 1 {
				l.Session, l.Evidence, l.View = claimed[0], TitleID, views[0]
			}
		}
		out = append(out, l)
	}
	return out
}

// Reconcile brings every pane in w in line with the sessions:
//
//   - a change, an end, an attachment or a detachment a writer did not get
//     to log is logged;
//   - a session whose host process is gone without SessionEnd ends (Exit);
//   - a pane whose stored attachment Links no longer confirms loses it
//     (Detach), and one Links attaches by title gains it (Attach);
//   - a projection that lags its session is rewritten.
//
// Every change is decided again under the pane's or session's lock against
// fresh state, so several tmag processes reconciling at once record each
// change once, and nothing newer than w is undone. Errors are not reported:
// the next reconciliation retries.
func Reconcile(ctx context.Context, env Env, w *World) {
	recs, err := env.Store.All()
	if err != nil {
		return
	}
	for _, p := range w.Panes {
		if a, ok := FromPane(p); ok && a.Change != "" {
			_ = env.settlePane(ctx, p)
		}
	}
	live, gone := Live(w, recs)
	for _, r := range gone {
		env.exit(ctx, r)
	}
	for _, r := range live {
		if r.Unlogged() {
			env.settle(ctx, r)
		}
	}
	linked := map[[2]string]bool{}
	for _, l := range Links(w, live) {
		if ctx.Err() != nil {
			return
		}
		p := l.Client.Pane
		linked[[2]string{p.Server, p.ID}] = true
		switch {
		case l.Stored:
			if projectionFromPane(p) != ProjectionOf(*l.Session) {
				a, _ := FromPane(p)
				_ = env.Show(ctx, p, a)
			}
		case l.Session != nil:
			_ = env.Attach(ctx, w, l.Client, *l.Session, l.Evidence)
		default:
			if a, ok := FromPane(p); ok {
				_ = env.detach(ctx, p, a)
			}
		}
	}
	// Panes whose agent is gone altogether, and options that are no
	// attachment at all, which tmux would still display.
	for _, p := range w.Panes {
		if a, ok := FromPane(p); ok && !linked[[2]string{p.Server, p.ID}] {
			_ = env.detach(ctx, p, a)
		} else if !ok && len(p.Options) > 0 {
			_ = env.clearBroken(ctx, p)
		}
	}
}

// clearBroken removes tmag options that hold no attachment, if they still
// hold none.
func (env Env) clearBroken(ctx context.Context, p tmux.Pane) error {
	srv := tmux.Server{Socket: p.Server}
	return Lock(ctx, env.Log.Dir, p.Server, p.ID, func() error {
		fresh, err := srv.Pane(ctx, p.ID)
		if err != nil {
			return err
		}
		if _, ok := FromPane(fresh); ok || len(fresh.Options) == 0 {
			return nil
		}
		return srv.ClearPaneOptions(ctx, fresh.ID)
	})
}

// Attach records that c shows rec, replacing whatever the pane showed, and
// writes the pane's projection. Under the pane's lock it proves the client
// again against a fresh process table before writing a new attachment, and
// gives up if the proof no longer holds.
func (env Env) Attach(ctx context.Context, w *World, c Client, rec session.Record, evidence string) error {
	srv := tmux.Server{Socket: c.Pane.Server}
	return Lock(ctx, env.Log.Dir, c.Pane.Server, c.Pane.ID, func() error {
		fresh, err := srv.Pane(ctx, c.Pane.ID)
		if err != nil {
			return err
		}
		if fresh.PID != c.Pane.PID {
			return nil
		}
		if evidence == TitleID {
			if _, shown := shows(fresh.Title, rec); !shown {
				return nil
			}
		}
		latest, ok, err := env.Store.Get(rec.Agent, rec.ID)
		if err != nil || !ok || latest.Ended != "" {
			return err
		}
		next := Attachment{Agent: rec.Agent, Session: rec.ID, PID: c.Process.PID, Start: c.Process.Start, Evidence: evidence}
		cur, had := FromPane(fresh)
		if had && cur.same(next) && cur.holds() {
			if err := env.finish(ctx, srv, fresh, cur); err != nil {
				return err
			}
			return writeProjection(ctx, srv, fresh.ID, ProjectionOf(latest))
		}
		pt, err := proc.Snapshot()
		if err != nil {
			return err
		}
		now, ok := (&World{Panes: []tmux.Pane{fresh}, Proc: pt}).ClientIn(fresh)
		if !ok || now.Agent != c.Agent || now.Process.PID != c.Process.PID || now.Process.Start != c.Process.Start {
			return nil // the client the proof was about no longer runs in the pane
		}
		// One write replaces the old attachment and marks both events; a
		// writer that dies leaves them for the next one to log.
		if had && cur.Change != "" {
			// Finish what the old attachment was in the middle of first.
			if err := env.finish(ctx, srv, fresh, cur); err != nil {
				return err
			}
			had = cur.holds()
		}
		next.Change = eventlog.Attach
		if had {
			next.Replaced = cur.Agent + " " + cur.Session
		}
		if err := write(ctx, srv, fresh.ID, fresh.Command, next, ProjectionOf(latest)); err != nil {
			return err
		}
		return env.finish(ctx, srv, fresh, next)
	})
}

// finish logs the event an attachment is in the middle of and completes it:
// an Attach is confirmed, a Detach clears the pane. The caller holds the
// pane's lock; fresh is the pane as read under it.
func (env Env) finish(ctx context.Context, srv tmux.Server, fresh tmux.Pane, a Attachment) error {
	switch a.Change {
	case eventlog.Attach:
		if agent, id, ok := strings.Cut(a.Replaced, " "); ok {
			if err := env.append(ctx, eventlog.Event{Agent: agent, Session: id, Event: eventlog.Detach, Pane: ref(fresh)}); err != nil {
				return err
			}
		}
		ev := eventlog.Event{Agent: a.Agent, Session: a.Session, Event: eventlog.Attach, Pane: ref(fresh), Evidence: a.Evidence}
		if rec, ok, _ := env.Store.Get(a.Agent, a.Session); ok {
			ev.State, ev.Kind, ev.Turn = rec.State(), rec.Kind(), rec.Turn
		}
		if err := env.append(ctx, ev); err != nil {
			return err
		}
		return srv.SetPaneOptions(ctx, fresh.ID, nil, []string{optChange, optReplaced})
	case eventlog.Detach:
		if err := env.append(ctx, eventlog.Event{Agent: a.Agent, Session: a.Session, Event: eventlog.Detach, Pane: ref(fresh)}); err != nil {
			return err
		}
		return srv.ClearPaneOptions(ctx, fresh.ID)
	}
	return nil
}

// unset detaches a from the pane: marked first, then logged, then cleared,
// so that a writer that dies leaves the mark for the next one to finish.
// The caller holds the pane's lock.
func (env Env) unset(ctx context.Context, srv tmux.Server, fresh tmux.Pane, a Attachment) error {
	if a.Change != eventlog.Detach {
		if err := srv.SetPaneOptions(ctx, fresh.ID, map[string]string{optChange: eventlog.Detach}, nil); err != nil {
			return err
		}
		a.Change = eventlog.Detach
	}
	return env.finish(ctx, srv, fresh, a)
}

// settlePane finishes the event a pane's attachment was left in the middle
// of, if it still is.
func (env Env) settlePane(ctx context.Context, p tmux.Pane) error {
	srv := tmux.Server{Socket: p.Server}
	return Lock(ctx, env.Log.Dir, p.Server, p.ID, func() error {
		fresh, err := srv.Pane(ctx, p.ID)
		if err != nil {
			return err
		}
		if cur, ok := FromPane(fresh); ok && cur.Change != "" {
			return env.finish(ctx, srv, fresh, cur)
		}
		return nil
	})
}

// Show rewrites the projection of a pane attached by a, from the session as
// it is now. It does nothing if the pane has been attached elsewhere since.
func (env Env) Show(ctx context.Context, p tmux.Pane, a Attachment) error {
	srv := tmux.Server{Socket: p.Server}
	return Lock(ctx, env.Log.Dir, p.Server, p.ID, func() error {
		fresh, err := srv.Pane(ctx, p.ID)
		if err != nil {
			return err
		}
		if cur, ok := FromPane(fresh); !ok || !cur.same(a) || !cur.holds() {
			return nil
		}
		rec, ok, err := env.Store.Get(a.Agent, a.Session)
		if err != nil || !ok {
			return err
		}
		return writeProjection(ctx, srv, fresh.ID, ProjectionOf(rec))
	})
}

// DetachAll removes every pane's attachment to the session, as when it ends.
func (env Env) DetachAll(ctx context.Context, w *World, agent, id string) error {
	var errs []error
	for _, p := range w.Panes {
		if a, ok := FromPane(p); ok && a.Agent == agent && a.Session == id {
			errs = append(errs, env.detach(ctx, p, a))
		}
	}
	return errors.Join(errs...)
}

// detach removes the pane's attachment if it is still a.
func (env Env) detach(ctx context.Context, p tmux.Pane, a Attachment) error {
	srv := tmux.Server{Socket: p.Server}
	return Lock(ctx, env.Log.Dir, p.Server, p.ID, func() error {
		fresh, err := srv.Pane(ctx, p.ID)
		if err != nil {
			return err // usually a pane closed since: its options went with it
		}
		if cur, ok := FromPane(fresh); ok && cur.same(a) {
			return env.unset(ctx, srv, fresh, cur)
		}
		return nil
	})
}

// exit ends a session that is gone: it finishes an end already decided, or
// ends a session whose host process exited, if it still has.
func (env Env) exit(ctx context.Context, r session.Record) {
	_ = env.Store.Lock(ctx, r.Agent, r.ID, func() error {
		cur, ok, err := env.Store.Get(r.Agent, r.ID)
		if err != nil || !ok {
			return err
		}
		if cur.Ended != "" {
			_, _, err := env.Store.Settle(ctx, env.Log, cur)
			return err
		}
		if cur.Host != r.Host {
			return nil // reported from another process since
		}
		return env.Store.End(ctx, env.Log, cur, eventlog.Event{Event: eventlog.Exit})
	})
}

// settle logs a change its writer did not get to log, if it still is not.
func (env Env) settle(ctx context.Context, r session.Record) {
	_ = env.Store.Lock(ctx, r.Agent, r.ID, func() error {
		cur, ok, err := env.Store.Get(r.Agent, r.ID)
		if err != nil || !ok {
			return err
		}
		_, _, err = env.Store.Settle(ctx, env.Log, cur)
		return err
	})
}

func (env Env) append(ctx context.Context, e eventlog.Event) error {
	return env.Log.Append(ctx, &e)
}

func ref(p tmux.Pane) *eventlog.PaneRef { return &eventlog.PaneRef{Server: p.Server, ID: p.ID} }
