package attach

import (
	"context"
	"errors"
	"time"

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
		if !ok || a.Agent != rec.Agent || a.Session != rec.ID {
			continue
		}
		if view, valid := w.Check(p, a, rec); valid {
			c, _ := w.ClientIn(p)
			out = append(out, Shown{Client: c, Attachment: a, View: view})
		}
	}
	return out
}

// Live splits sessions into those that run and those whose host process
// exited without SessionEnd.
func Live(w *World, recs []session.Record) (live, gone []session.Record) {
	for _, r := range recs {
		if r.Host.PID > 0 && !w.Proc.Alive(r.Host.PID, r.Host.Start) {
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
		if a, ok := FromPane(c.Pane); ok {
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
//   - a session whose host process is gone without SessionEnd ends (Exit);
//   - a pane whose stored attachment Links no longer confirms loses it
//     (Detach), and one Links attaches by title gains it (Attach);
//   - a projection that lags its session is rewritten.
//
// Every change is decided again under the pane's or session's lock against
// fresh state, so several tmag processes reconciling at once make each change
// once, and nothing newer than w is undone. A session whose record cannot be
// read is left alone. The first error is returned.
func Reconcile(ctx context.Context, env Env, w *World) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.Proc == nil || !w.Proc.Complete() {
		return errors.New("process table incomplete; reconciliation requires complete evidence")
	}
	recs, _, err := env.Store.All(ctx)
	if err != nil {
		return err
	}
	live, gone := Live(w, recs)
	for _, r := range gone {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := env.exit(ctx, r); err != nil {
			return err
		}
	}
	linked := map[[2]string]bool{}
	for _, l := range Links(w, live) {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := l.Client.Pane
		linked[[2]string{p.Server, p.ID}] = true
		switch {
		case l.Stored:
			if projectionFromPane(p) != ProjectionOf(*l.Session) {
				a, _ := FromPane(p)
				if err := env.Show(ctx, p, a); err != nil {
					return err
				}
			}
		case l.Session != nil:
			if err := env.Attach(ctx, w, l.Client, *l.Session, l.Evidence); err != nil {
				return err
			}
		default:
			if a, ok := FromPane(p); ok {
				if err := env.detach(ctx, p, a); err != nil {
					return err
				}
			}
		}
	}
	// Panes whose agent is gone altogether, and options that are no
	// attachment at all, which tmux would still display.
	for _, p := range w.Panes {
		if a, ok := FromPane(p); ok && !linked[[2]string{p.Server, p.ID}] {
			if err := env.detach(ctx, p, a); err != nil {
				return err
			}
		} else if !ok && len(p.Options) > 0 {
			if err := env.clearBroken(ctx, p); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
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
// gives up if the proof no longer holds. A title inference never replaces a
// process proof of the same client and session, which a reconciliation may
// not have seen yet. The pane changes first, then the events are logged: a
// Detach of the session the pane stops showing, then the Attach; a change
// of evidence or client alone logs only the Attach.
func (env Env) Attach(ctx context.Context, w *World, c Client, rec session.Record, evidence string) error {
	return env.attach(ctx, w, c, rec, evidence, nil)
}

// AttachFromHook is Attach for a hook that proved its own host's pane on the
// full path. It may publish the pane hint only if the record's generation is
// still the one the hook read before it listed panes.
func (env Env) AttachFromHook(ctx context.Context, w *World, c Client, rec session.Record, evidence string, generation uint64) error {
	return env.attach(ctx, w, c, rec, evidence, &generation)
}

func (env Env) attach(ctx context.Context, w *World, c Client, rec session.Record, evidence string, hintGeneration *uint64) error {
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
		latest, ok, err := env.Store.Get(ctx, rec.Agent, rec.ID)
		if err != nil || !ok {
			return err
		}
		next := Attachment{Agent: rec.Agent, Session: rec.ID, PID: c.Process.PID, Start: c.Process.Start, Evidence: evidence}
		cur, had := FromPane(fresh)
		if had && cur.keeps(next) {
			pub, err := env.prepareAttach(ctx, fresh, next)
			if err != nil || !pub.found {
				return err
			}
			if err := writeProjection(ctx, srv, fresh.ID, ProjectionOf(latest)); err != nil {
				return err
			}
			return env.storeHint(ctx, fresh, next, w, hintGeneration, ProjectionOf(latest), pub)
		}
		pt, err := proc.SnapshotContext(ctx)
		if err != nil {
			return err
		}
		now, ok := (&World{Panes: []tmux.Pane{fresh}, Proc: pt}).ClientIn(fresh)
		if !ok || now.Agent != c.Agent || now.Process.PID != c.Process.PID || now.Process.Start != c.Process.Start {
			return nil // the client the proof was about no longer runs in the pane
		}
		replaced := had && (cur.Agent != next.Agent || cur.Session != next.Session)
		if replaced {
			// Conservative invalidation happens before publication; the Detach
			// event waits until the replacement pane write succeeds.
			if err := env.clearHint(ctx, fresh, cur); err != nil {
				return err
			}
		}
		// The Attach reports the session in full, so it is read, its token
		// published, its pane written and its event logged under the session's
		// lock. A hook cannot capture the new token until the pane is visible.
		ev := eventlog.Event{Agent: next.Agent, Session: next.Session, Event: eventlog.Attach, Pane: ref(fresh), Evidence: next.Evidence}
		return env.Store.Lock(ctx, next.Agent, next.Session, func() error {
			rec, ok, err := env.Store.Get(ctx, next.Agent, next.Session)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			before := rec.AttachGeneration
			after := nextAttachGeneration(&rec)
			pub := publication{before: before, after: after, found: true}
			if rec.Pane != nil && (rec.Pane.Server != fresh.Server || rec.Pane.ID != fresh.ID) {
				rec.Pane = nil
			}
			if rec.Pane != nil && next.Evidence != Process {
				rec.Pane = nil
			}
			if err := env.Store.Put(ctx, rec); err != nil {
				return err
			}
			if err := write(ctx, srv, fresh.ID, fresh.Command, next, ProjectionOf(rec)); err != nil {
				return err
			}
			rec.Report(&ev)
			if publishHint(&rec, fresh, next, w, hintGeneration, pub) {
				if err := env.Store.Put(ctx, rec); err != nil {
					return err
				}
			}
			if replaced {
				if err := env.append(ctx, eventlog.Event{Agent: cur.Agent, Session: cur.Session, Event: eventlog.Detach, Pane: ref(fresh)}); err != nil {
					return err
				}
			}
			return env.append(ctx, ev)
		})
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
		if cur, ok := FromPane(fresh); !ok || cur != a {
			return nil
		}
		rec, ok, err := env.Store.Get(ctx, a.Agent, a.Session)
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

// detach removes the pane's attachment if it is still a, then logs it.
func (env Env) detach(ctx context.Context, p tmux.Pane, a Attachment) error {
	srv := tmux.Server{Socket: p.Server}
	return Lock(ctx, env.Log.Dir, p.Server, p.ID, func() error {
		fresh, err := srv.Pane(ctx, p.ID)
		if err != nil {
			return err // usually a pane closed since: its options went with it
		}
		if cur, ok := FromPane(fresh); !ok || cur != a {
			return nil
		}
		if err := srv.ClearPaneOptions(ctx, fresh.ID); err != nil {
			return err
		}
		if err := env.clearHint(ctx, fresh, a); err != nil {
			return err
		}
		return env.append(ctx, eventlog.Event{Agent: a.Agent, Session: a.Session, Event: eventlog.Detach, Pane: ref(fresh)})
	})
}

// hintAllowed reports whether a process proof on server may carry the
// session's hint: no proven attachment of the session lives on another server,
// which the fast path could not update in one invocation.
func hintAllowed(w *World, server string, rec session.Record) bool {
	if w == nil || w.Proc == nil {
		return true
	}
	for _, s := range w.Attached(rec) {
		if s.Client.Pane.Server != server {
			return false
		}
	}
	return true
}

// publication is the generation range one Attach published.
type publication struct {
	before, after uint64
	found         bool
}

// nextAttachGeneration seeds a new record's generation from the clock and
// increments an existing token, so a recreated record cannot reuse an old
// hook's token.
func nextAttachGeneration(rec *session.Record) uint64 {
	if rec.AttachGeneration == 0 {
		rec.AttachGeneration = uint64(time.Now().UnixNano())
	} else {
		rec.AttachGeneration++
	}
	return rec.AttachGeneration
}

// prepareAttach durably bumps the generation and clears a hint for another
// pane before this Attach makes its pane visible. The caller holds the pane
// lock and then takes the session lock, preserving pane-before-session order.
func (env Env) prepareAttach(ctx context.Context, p tmux.Pane, a Attachment) (publication, error) {
	var pub publication
	err := env.Store.Lock(ctx, a.Agent, a.Session, func() error {
		rec, ok, err := env.Store.Get(ctx, a.Agent, a.Session)
		if err != nil || !ok {
			return err
		}
		pub.found = true
		pub.before = rec.AttachGeneration
		pub.after = nextAttachGeneration(&rec)
		if rec.Pane != nil && (rec.Pane.Server != p.Server || rec.Pane.ID != p.ID) {
			rec.Pane = nil
		}
		if rec.Pane != nil && a.Evidence != Process {
			rec.Pane = nil
		}
		return env.Store.Put(ctx, rec)
	})
	return pub, err
}

// publishHint updates the hint for the pane an Attach just published. Any
// other pane's Attach clears the hint; only a hook's own proof may set it, and
// only while its generation range is still current and its world has no other
// server's attachment.
func publishHint(rec *session.Record, p tmux.Pane, a Attachment, w *World, hintGeneration *uint64, pub publication) bool {
	if !pub.found || rec.AttachGeneration != pub.after {
		return false // another Attach published after this one
	}
	if rec.Pane != nil && (rec.Pane.Server != p.Server || rec.Pane.ID != p.ID) {
		rec.Pane = nil
		return true
	}
	if a.Evidence != Process || hintGeneration == nil {
		if a.Evidence != Process && rec.Pane != nil {
			rec.Pane = nil
			return true
		}
		return false
	}
	if !hintAllowed(w, p.Server, *rec) {
		if rec.Pane != nil {
			rec.Pane = nil
			return true
		}
		return false
	}
	if pub.before == 0 || *hintGeneration != pub.before {
		return false
	}
	next := &session.PaneHint{Server: p.Server, ID: p.ID, PID: a.PID, Start: a.Start}
	if session.SamePaneHint(rec.Pane, next) {
		return false
	}
	rec.Pane = next
	return true
}

// storeHint updates the hint, generation and projection for a kept process
// attachment, rereading the record so a racing hook's newer state is not left
// behind. The caller holds the pane's lock.
func (env Env) storeHint(ctx context.Context, p tmux.Pane, a Attachment, w *World, hintGeneration *uint64, shown Projection, pub publication) error {
	return env.Store.Lock(ctx, a.Agent, a.Session, func() error {
		rec, ok, err := env.Store.Get(ctx, a.Agent, a.Session)
		if err != nil || !ok {
			return err
		}
		if publishHint(&rec, p, a, w, hintGeneration, pub) {
			if err := env.Store.Put(ctx, rec); err != nil {
				return err
			}
		}
		if ProjectionOf(rec) != shown {
			return writeProjection(ctx, tmux.Server{Socket: p.Server}, p.ID, ProjectionOf(rec))
		}
		return nil
	})
}

// clearHint removes a's hint when p no longer shows it. The caller holds the
// pane's lock.
func (env Env) clearHint(ctx context.Context, p tmux.Pane, a Attachment) error {
	hint := &session.PaneHint{Server: p.Server, ID: p.ID, PID: a.PID, Start: a.Start}
	return env.Store.Lock(ctx, a.Agent, a.Session, func() error {
		rec, ok, err := env.Store.Get(ctx, a.Agent, a.Session)
		if err != nil || !ok {
			return err
		}
		if !session.SamePaneHint(rec.Pane, hint) {
			return nil
		}
		rec.Pane = nil
		return env.Store.Put(ctx, rec)
	})
}

// exit ends a session whose host process exited without SessionEnd, if it
// still has.
func (env Env) exit(ctx context.Context, r session.Record) error {
	return env.Store.Lock(ctx, r.Agent, r.ID, func() error {
		cur, ok, err := env.Store.Get(ctx, r.Agent, r.ID)
		if err != nil || !ok {
			return err
		}
		if cur.Host != r.Host {
			return nil // reported from another process since
		}
		if cur.Host.PID <= 0 || cur.Host.Start == "" {
			return nil
		}
		pt, err := proc.SnapshotContext(ctx)
		if err != nil {
			return err
		}
		if pt.Alive(cur.Host.PID, cur.Host.Start) {
			return nil
		}
		return env.Store.End(ctx, env.Log, cur, eventlog.Event{Event: eventlog.Exit})
	})
}

func (env Env) append(ctx context.Context, e eventlog.Event) error {
	return env.Log.Append(ctx, &e)
}

func ref(p tmux.Pane) *eventlog.PaneRef { return &eventlog.PaneRef{Server: p.Server, ID: p.ID} }
