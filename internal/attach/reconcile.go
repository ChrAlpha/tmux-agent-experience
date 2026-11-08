package attach

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Env is where attachment changes are recorded.
type Env struct {
	Log   *eventlog.Log
	Store session.Store
	// Snapshot rechecks host evidence under the session lock; nil uses the OS.
	Snapshot func(context.Context) (*proc.Table, error)
	// PaneServer limits explicit pane repair; image capture remains realm-wide.
	PaneServer string
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

// Live excludes only settled end decisions. Host absence is reconciled under
// the session lock and must not silently remove sessions from a read image.
func Live(w *World, recs []session.Record) (live, gone []session.Record) {
	for _, r := range recs {
		if r.Ended != "" {
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

// Fault is a bounded metadata-only repair failure.
type Fault struct{ Operation, Category, Agent, ID string }
type ReconcileReport struct {
	Examined, Repaired, Deferred uint64
	FaultCount                   uint64
	Faults                       []Fault
	Complete                     bool
	// Err is an in-memory error tree for the repair caller, never persisted.
	Err error `json:"-"`
}

func (r *ReconcileReport) add(ctx context.Context, operation string, err error, agent, id string) {
	r.Examined++
	if err == nil {
		r.Repaired++
		return
	}
	r.Complete = false
	expired := ctx.Err()
	remainder := contextio.WithoutContextError(err, expired)
	if expired != nil && remainder == nil {
		r.Deferred++
		return
	}
	r.Err = errors.Join(r.Err, remainder)
	r.FaultCount++
	category := "io"
	if contextio.WithoutContextError(remainder, context.DeadlineExceeded) == nil {
		category = "timeout"
	}
	var issue *session.RecordIssue
	if errors.As(err, &issue) {
		category = issue.Category
	}
	if len(r.Faults) < 16 {
		r.Faults = append(r.Faults, Fault{operation, category, agent, id})
	}
}

// Reconcile repairs valid records even when a complete v1 image is unavailable.
// No partial catalog can establish absence or unique title evidence.
func Reconcile(ctx context.Context, env Env, w *World) ReconcileReport {
	c, err := env.Store.ScanContext(ctx)
	report := ReconcileReport{Complete: err == nil && c.Complete && c.IssueCount == 0}
	if err != nil {
		report.add(ctx, "scan", err, "", "")
	}
	for _, i := range c.Issues {
		report.add(ctx, "scan", &i, i.Agent, i.ID)
	}
	if w == nil || w.Proc == nil || !w.Proc.Complete() {
		report.Complete = false
		return report
	}
	for _, r := range c.Records {
		if ctx.Err() != nil {
			report.Complete = false
			return report
		}
		report.add(ctx, "session", env.RepairRecord(ctx, w, r), r.Agent, r.ID)
	}
	// Record repair can end sessions: read again before attaching any pane.
	c, err = env.Store.ScanContext(ctx)
	if err != nil {
		report.add(ctx, "scan", err, "", "")
	}
	pr := env.RepairPanes(ctx, w, c, w.Panes)
	report.Examined += pr.Examined
	report.Repaired += pr.Repaired
	report.Deferred += pr.Deferred
	report.FaultCount += pr.FaultCount
	report.Err = errors.Join(report.Err, pr.Err)
	report.Complete = report.Complete && pr.Complete
	for _, f := range pr.Faults {
		if len(report.Faults) < 16 {
			report.Faults = append(report.Faults, f)
		}
	}
	return report
}

// RepairRecord settles a pending publication, then freshly checks runtime death.
func (env Env) RepairRecord(ctx context.Context, w *World, r session.Record) error {
	proven := w != nil && w.Proc != nil && w.Proc.Complete() && r.Host.PID > 0 && r.Host.Start != ""
	suspect := proven && !w.Proc.Alive(r.Host.PID, r.Host.Start)
	verified := false
	if proven {
		if p, ok := w.Proc.Get(r.Host.PID); ok && !p.Uncertain && p.Start == r.Host.Start {
			verified = true
		}
	}
	return env.exit(ctx, r, suspect, verified)
}

// RepairPanes examines only selected panes; w and c remain the full proof set.
func (env Env) RepairPanes(ctx context.Context, w *World, c session.Catalog, panes []tmux.Pane) ReconcileReport {
	report := ReconcileReport{Complete: true}
	if !c.Complete || c.IssueCount != 0 {
		report.Complete = false
		// Diagnostic presentation only: do not change attachment or event markers.
		for _, p := range panes {
			if ctx.Err() != nil {
				return report
			}
			if a, ok := FromPane(p); ok {
				_, found, err := env.Store.GetContext(ctx, a.Agent, a.Session)
				if err != nil || !found {
					report.add(ctx, "suppress", env.suppress(ctx, p, a), a.Agent, a.Session)
				}
			}
		}
		return report
	}
	live, _ := Live(w, c.Records)
	links := map[[2]string]Link{}
	for _, l := range Links(w, live) {
		links[[2]string{l.Client.Pane.Server, l.Client.Pane.ID}] = l
	}
	for _, p := range panes {
		if ctx.Err() != nil {
			report.Complete = false
			report.Deferred++
			return report
		}
		a, had := FromPane(p)
		if had && a.Change != "" {
			if err := env.settlePane(ctx, p); err != nil {
				report.add(ctx, "settle_pane", err, a.Agent, a.Session)
				continue
			}
		}
		l, linked := links[[2]string{p.Server, p.ID}]
		var err error
		switch {
		case linked && l.Stored:
			if projectionFromPane(p) != ProjectionOf(*l.Session) || p.Options["unavailable"] != "" {
				err = env.Show(ctx, p, a)
			}
		case linked && l.Session != nil:
			err = env.Attach(ctx, w, l.Client, *l.Session, l.Evidence)
		case had:
			err = env.detach(ctx, p, a)
		case len(p.Options) > 0:
			err = env.clearBroken(ctx, p)
		}
		report.add(ctx, "pane", err, a.Agent, a.Session)
	}
	return report
}

// suppress is an ephemeral diagnostic overlay, not a projection/state change.
// Restoring readable metadata lets Show remove it without consuming markers.
func (env Env) suppress(ctx context.Context, p tmux.Pane, a Attachment) error {
	return Lock(ctx, env.Log.Dir, p.Server, p.ID, func() error {
		srv := tmux.Server{Socket: p.Server}
		fresh, err := srv.Pane(ctx, p.ID)
		if err != nil {
			return err
		}
		if cur, ok := FromPane(fresh); !ok || !cur.same(a) {
			return nil
		}
		if _, found, err := env.Store.GetContext(ctx, a.Agent, a.Session); err == nil && found {
			return nil
		}
		return srv.SetPaneOptions(ctx, p.ID, map[string]string{"unavailable": "on"}, nil)
	})
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
		latest, ok, err := env.Store.GetContext(ctx, rec.Agent, rec.ID)
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
		pt, err := proc.SnapshotContext(ctx)
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
// pane's lock, and no session's; fresh is the pane as read under it.
func (env Env) finish(ctx context.Context, srv tmux.Server, fresh tmux.Pane, a Attachment) error {
	switch a.Change {
	case eventlog.Attach:
		if agent, id, ok := strings.Cut(a.Replaced, " "); ok {
			if err := env.append(ctx, eventlog.Event{Agent: agent, Session: id, Event: eventlog.Detach, Pane: ref(fresh)}); err != nil {
				return err
			}
		}
		// The event reports the session in full, so it is read and logged
		// under the session's lock: a hook changing the session meanwhile
		// logs after it, never before with this event reporting less.
		ev := eventlog.Event{Agent: a.Agent, Session: a.Session, Event: eventlog.Attach, Pane: ref(fresh), Evidence: a.Evidence}
		if err := env.Store.Lock(ctx, a.Agent, a.Session, func() error {
			rec, ok, err := env.Store.GetContext(ctx, a.Agent, a.Session)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("records_unavailable: pending attachment has no readable record")
			}
			rec.Report(&ev)
			return env.append(ctx, ev)
		}); err != nil {
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
		rec, ok, err := env.Store.GetContext(ctx, a.Agent, a.Session)
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
func (env Env) exit(ctx context.Context, r session.Record, suspect, verified bool) error {
	return env.Store.Lock(ctx, r.Agent, r.ID, func() error {
		cur, ok, err := env.Store.Current(ctx, env.Log, r.Agent, r.ID)
		if err != nil || !ok {
			return err
		}
		if cur.Host != r.Host {
			return nil
		}
		// Optional observation must not immortalize legacy records. The selected
		// realm already owns their Host contract; explicit foreign evidence refuses.
		realm, err := proc.RealmContext(ctx)
		if err != nil {
			return err
		}
		if cur.Observation != nil && cur.Observation.Realm != "" && cur.Observation.Realm != realm {
			return nil
		}
		if cur.Host.PID <= 0 || cur.Host.Start == "" {
			return nil
		}
		if suspect {
			snapshot := env.Snapshot
			if snapshot == nil {
				snapshot = proc.SnapshotContext
			}
			pt, err := snapshot(ctx)
			if err != nil {
				return err
			}
			if !pt.Complete() {
				return errors.New("world_unavailable: incomplete process table")
			}
			if !pt.Alive(cur.Host.PID, cur.Host.Start) {
				return env.Store.End(ctx, env.Log, cur, eventlog.Event{Event: eventlog.Exit})
			}
			if p, ok := pt.Get(cur.Host.PID); ok && !p.Uncertain && p.Start == cur.Host.Start {
				verified = true
			}
		}
		// A live shared service proves process lifetime only, never thread activity.
		if verified && cur.Observation != nil && cur.Observation.Host != "verified" {
			o := *cur.Observation
			o.Host = "verified"
			cur.Observation = &o
			_, err = env.Store.Save(ctx, env.Log, cur, eventlog.Event{Event: "Observation", Time: time.Now().UTC()})
			return err
		}
		return nil
	})
}

func (env Env) append(ctx context.Context, e eventlog.Event) error {
	return env.Log.Append(ctx, &e)
}

func ref(p tmux.Pane) *eventlog.PaneRef { return &eventlog.PaneRef{Server: p.Server, ID: p.ID} }
