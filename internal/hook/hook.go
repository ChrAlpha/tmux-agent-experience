// Package hook handles one agent hook invocation: map the event, apply it to
// the session, update the panes its record lists, or prove the pane the
// hook runs in, alert the person, and log what changed.
package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Deadline bounds a whole hook invocation. Agents wait for synchronous hooks,
// some of which run on every tool call.
const Deadline = 3 * time.Second

// SlowAfter is how long a successful hook may take before hook.log records it
// as slow: agents wait for hooks, on every tool call too.
const SlowAfter = 500 * time.Millisecond

// maxPayload bounds how much hook input is read; Stop payloads embed the last
// assistant message, which is decoded away but must still be consumed.
const maxPayload = 64 << 20

// ContactInterval is how often a hook that changes nothing else records its
// contact with the session (last_hook_at). Contact alone is logged as no
// event; views show it as of the record.
const ContactInterval = 10 * time.Second

// Input is one hook invocation.
type Input struct {
	Agent   string
	Payload io.Reader
	Log     *eventlog.Log
	// PID starts the ancestry walk; normally os.Getpid().
	PID int
	Now func() time.Time
	// Request is the id an answer hook waits under, for the prompt this
	// event opens; "" for a plain hook.
	Request string
	// Sources are where the hook reads what it proves; the zero value reads
	// the real process table and tmux servers.
	Sources Sources
}

// Sources are where a hook reads what it proves: its own ancestry, every
// tmux server's panes and the whole process table. A nil field reads the
// real one.
type Sources struct {
	Lineage   func(ctx context.Context, pid int) (*proc.Table, error)
	Panes     func(ctx context.Context) ([]tmux.Pane, error)
	Processes func(ctx context.Context) (*proc.Table, error)
}

// world lists every server's panes, then reads the process table, so that a
// proof never checks an attachment against a table older than it. complete
// is false when the table could not be read: the hook then proves nothing.
func (s Sources) world(ctx context.Context) (w *attach.World, complete bool, err error) {
	w = &attach.World{}
	if s.Panes != nil {
		w.Panes, err = s.Panes(ctx)
	} else {
		err = w.LoadPanes(ctx)
	}
	if err != nil {
		return nil, false, err
	}
	read := proc.Snapshot
	if s.Processes != nil {
		read = s.Processes
	}
	if w.Proc, err = read(ctx); err != nil {
		w.Proc = nil
		return w, false, ctx.Err()
	}
	return w, w.Proc.Complete(), nil
}

// self is what a hook proves of itself from its own ancestry: a handful of
// /proc entries, not every process. The table is incomplete by construction,
// so it can never authorize reconciliation.
type self struct {
	table *proc.Table
	host  proc.Host
	known bool // the hook runs under an agent's client or service
	tmux  bool // and a tmux server is among its ancestors
}

func (s Sources) self(ctx context.Context, pid int, p adapter.Payload) self {
	read := proc.Lineage
	if s.Lineage != nil {
		read = s.Lineage
	}
	t, err := read(ctx, pid)
	if err != nil {
		return self{}
	}
	me := self{table: t, tmux: t.UnderTmux(pid)}
	me.host, me.known = t.HostOf(pid)
	if me.known && p.Mode != "" && p.Mode != "tui" {
		// Pi and oh-my-pi say when they run for a program rather than a
		// person, which their process does not show: such a session runs as
		// a service, which no pane shows and no channel controls. The claim
		// only takes away, so it needs no proof.
		me.host.Role = proc.Service
	}
	return me
}

// listed is the pane the record lists for the hook's own host.
func (me self) listed(rec session.Record) (session.Shown, bool) {
	if !me.known {
		return session.Shown{}, false
	}
	return rec.ShownFor(me.host.Process.PID, me.host.Process.Start)
}

// unproven reports whether the hook's host is a client in a pane the record
// does not list: only a process proof, on the full path, can attach it.
// Service hosts and clients outside tmux run in no pane.
func (me self) unproven(rec session.Record) bool {
	_, ok := me.listed(rec)
	return me.known && me.host.Role == proc.Terminal && me.tmux && !ok
}

// Run processes a hook using the earlier of the parent's budget and
// Deadline, including input. It never writes to stdout: agents inject hook
// stdout into the model context. A payload that may block must stop on
// cancellation: the CLI owns its inherited input with contextio.Own.
//
// The session's record and event are saved before the hook asks tmux
// anything. The panes the record lists are then updated without listing panes
// or the process table, when the projection changed; only a session's start
// or end, or a host in a pane the record does not list, takes the full path.
func Run(parent context.Context, in Input) error {
	ctx, cancel := context.WithTimeout(parent, Deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !proc.IsAgent(in.Agent) {
		return fmt.Errorf("unknown agent %q", in.Agent)
	}
	data, err := contextio.ReadAtMost(in.Payload, maxPayload)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err // input that outlived the budget changes nothing
	}
	p, err := adapter.Decode(data)
	if err != nil {
		return err
	}
	tr := adapter.Map(in.Agent, p)
	if tr.Action == adapter.Ignore {
		return nil
	}
	if tr.Action == adapter.Ask {
		tr.Request, tr.Waits = in.Request, in.Request != ""
	}
	if !session.ValidID(p.SessionID) {
		return fmt.Errorf("unusable session id %q", p.SessionID)
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	me := in.Sources.self(ctx, in.PID, p)
	env := attach.Env{Log: in.Log, Store: session.Store{Dir: in.Log.Dir}}
	prev, rec, wrote, err := save(ctx, env, in, p, tr, me, now)
	if err != nil {
		return err
	}
	// An answer hook whose prompt closed (answered in the terminal, the call
	// ran, the turn ended) would otherwise wait on: the agent ignores what it
	// prints now.
	var closed []string
	for _, q := range prev.Requests {
		if _, open := rec.Request(q.ID); !open && q.Hook {
			closed = append(closed, q.ID)
		}
	}
	answer.Close(ctx, in.Log.Dir, closed...)

	if tr.Action == adapter.End || reconcileEvent(p.HookEventName) || me.unproven(rec) {
		return prove(ctx, env, in, p, prev, rec, wrote, me)
	}
	if attach.ProjectionOf(prev) == attach.ProjectionOf(rec) {
		return nil // no tmux process at all
	}
	ring := Alert(prev.State(), rec.State())
	var options []string
	if ring != "" {
		options = []string{notify.Option, "@tmag-alert"}
	}
	written, refused, err := env.Project(ctx, rec, options...)
	if err != nil {
		return err
	}
	if own, ok := me.listed(rec); ok && slices.Contains(refused, own) {
		// The hook's own pane no longer shows the session as listed: prove
		// it again.
		return prove(ctx, env, in, p, prev, rec, wrote, me)
	}
	if ring == "" || slices.ContainsFunc(written, func(w attach.Projected) bool { return w.Newer }) {
		return nil // a newer state's own hook alerts for it
	}
	known := map[string]attach.Projected{}
	var shown []tmux.Pane
	for _, w := range written {
		if w.Read {
			known[w.Pane.Server] = w
		}
		shown = append(shown, w.Pane)
	}
	for _, w := range written {
		alertWith(known[w.Pane.Server].Options["@tmag-alert"], w.Pane, ring)
	}
	return tell(ctx, rec, shown, ring, known)
}

// save applies the event to the session's record and logs what it reports,
// under the session's lock. A record that cannot be read is the hook's to
// replace: the session's state comes from its hooks, and this one reports
// it anew. wrote is false when the hook changed nothing worth saving.
func save(ctx context.Context, env attach.Env, in Input, p adapter.Payload, tr adapter.Transition, me self, now func() time.Time) (prev, rec session.Record, wrote bool, err error) {
	get := func() (session.Record, bool, error) {
		cur, found, err := env.Store.Get(ctx, in.Agent, p.SessionID)
		var fault *session.RecordError
		if errors.As(err, &fault) {
			RecordError(ctx, in.Log.Dir, in.Agent, err)
			found, err = false, nil
		}
		if err == nil && !found {
			cur = session.Record{Agent: in.Agent, ID: p.SessionID}
		}
		return cur, found, err
	}
	contact := now().UTC()
	ev := eventlog.Event{Agent: in.Agent, Session: p.SessionID, Event: p.HookEventName, Cwd: p.Cwd, LastHookAt: contact}
	if tr.Action == adapter.End {
		// Whatever the agent calls it, the contract's end of a session is
		// SessionEnd, which followers drop the session on.
		ev.Event = eventlog.End
		err = env.Store.Lock(ctx, in.Agent, p.SessionID, func() error {
			cur, _, err := get()
			if err != nil {
				return err
			}
			prev = cur
			return env.Store.End(ctx, in.Log, cur, ev)
		})
		return prev, session.Record{}, err == nil, err
	}
	// The inbox a hook's agent names, and the process that serves it, are
	// recorded for a send to prove later. The hook never connects to it.
	inbox := inboxFor(me.table, in.PID, in.Agent, inboxOf(in.Agent, p, os.Getenv))
	err = env.Store.Lock(ctx, in.Agent, p.SessionID, func() error {
		cur, found, err := get()
		if err != nil {
			return err
		}
		base := cur
		if p.Cwd != "" {
			base.Cwd = p.Cwd
		}
		if me.known {
			base.Host = session.Host{PID: me.host.Process.PID, Start: me.host.Process.Start, Role: me.host.Role}
			if status := statusFileOf(in.Agent, me.host.Process.PID, os.Getenv); status != "" {
				base.StatusFile = status
			}
		}
		if p.TranscriptPath != "" {
			base.Transcript = p.TranscriptPath
		}
		if inbox != nil {
			base.Inbox = inbox
		}
		if class := p.PermissionClass(); class != "" {
			base.Permission = class
		}
		next, changed := session.Apply(base, tr, now())
		if found && !changed && base.Same(cur) && contact.Sub(cur.LastHookAt) < ContactInterval {
			prev, rec = cur, cur // nothing to write
			return nil
		}
		if contact.After(next.LastHookAt) {
			next.LastHookAt = contact
		}
		// Calls and other background work change without an event; whatever
		// an event reports of the session (session.Report) is logged.
		if err := env.Store.Save(ctx, in.Log, cur, next, ev); err != nil {
			return err
		}
		prev, rec, wrote = cur, next, true
		return nil
	})
	return prev, rec, wrote, err
}

// prove takes the full path: it lists every server's panes and reads the
// process table, attaches the hook's own pane when the hook proves its host
// is the pane's own agent, updates the other panes that show the session,
// and alerts. A session's start or end also reconciles every pane, last, on
// a process table read after the panes: a table older than a pane could miss
// a client that attached meanwhile, and reconciliation would detach it.
func prove(ctx context.Context, env attach.Env, in Input, p adapter.Payload, prev, rec session.Record, wrote bool, me self) (err error) {
	w, complete, err := in.Sources.world(ctx)
	if err != nil {
		return err
	}
	if complete && reconcileEvent(p.HookEventName) {
		defer func() {
			err = errors.Join(err, attach.Reconcile(ctx, env, w))
		}()
	}
	if rec.ID == "" {
		return env.DetachAll(ctx, w, in.Agent, p.SessionID)
	}
	if !complete {
		return nil
	}
	var shown []tmux.Pane
	if c, ok := w.ClientOf(me.host); me.known && ok {
		if _, listed := me.listed(rec); !listed || !c.Shows(rec) {
			if err := env.Attach(ctx, w, c, rec, attach.Process); err != nil {
				return err
			}
		} else if wrote {
			if a, ok := attach.FromPane(c.Pane); ok {
				if err := env.Show(ctx, c.Pane, a); err != nil {
					return err
				}
			}
		}
		shown = append(shown, c.Pane)
	}
	if !wrote {
		return nil
	}
	for _, s := range w.Attached(rec) {
		if len(shown) > 0 && s.Client.Pane.Server == shown[0].Server && s.Client.Pane.ID == shown[0].ID {
			continue // the hook's own pane, written above
		}
		if err := env.Show(ctx, s.Client.Pane, s.Attachment); err != nil {
			return err
		}
		shown = append(shown, s.Client.Pane)
	}
	ring := Alert(prev.State(), rec.State())
	for _, pane := range shown {
		alert(ctx, tmux.Server{Socket: pane.Server}, pane, ring)
	}
	return tell(ctx, rec, shown, ring, nil)
}

// reconcileEvent reports whether a hook reconciles the world: session start
// and end only, in each agent's own event names, so tool events stay off the
// full scan.
func reconcileEvent(event string) bool {
	switch event {
	case "SessionStart", "SessionEnd", "session_start", "session_shutdown", "session_switch":
		return true
	}
	return false
}

// Alert names the change worth a bell, or "": "blocked" when an agent starts
// waiting for the user, "finished" when a turn ends.
func Alert(prev, next string) string {
	switch {
	case next == adapter.Blocked && prev != adapter.Blocked:
		return "blocked"
	case next == adapter.Idle && prev == adapter.Working:
		return "finished"
	}
	return ""
}

// alert rings the pane's bell when the tmux plugin enabled this kind of alert
// (@tmag-alert) and nobody is looking at the pane. tmux turns the bell into the
// window's alert flag, which prefix + M-n jumps to.
func alert(ctx context.Context, srv tmux.Server, pane tmux.Pane, kind string) {
	if kind == "" || pane.Watched {
		return
	}
	enabled, _ := srv.Option(ctx, "@tmag-alert")
	alertWith(enabled, pane, kind)
}

// alertWith rings a pane's bell when enabled names this kind of alert and
// nobody is looking at the pane.
func alertWith(enabled string, pane tmux.Pane, kind string) {
	if kind != "" && !pane.Watched && alertNames(enabled, kind) {
		_ = pane.Bell()
	}
}

func alertNames(enabled, kind string) bool {
	for _, name := range strings.Split(enabled, ",") {
		if strings.TrimSpace(name) == kind {
			return true
		}
	}
	return false
}

// tell runs the person's @tmag-notify for an alert, for every session,
// unless they are at the terminal that shows it: a pane shows the session on
// a tmux server one of whose clients had their input within notify.Present,
// and the bell reaches them there. A session no pane shows rings no bell, so
// its alerts always run the command. Each distinct command set on a server
// whose @tmag-alert names the alert runs once, detached, so the hook does not
// wait for the network. A server known from Project, which read its options
// and client activity with its first pane, is not asked again; every other
// server costs one option read.
func tell(ctx context.Context, rec session.Record, shown []tmux.Pane, kind string, known map[string]attach.Projected) error {
	if kind == "" {
		return nil
	}
	servers, err := tmux.Servers(ctx)
	if err != nil {
		return err
	}
	var commands []string
	for _, srv := range servers {
		values := known[srv.Socket].Options
		if _, ok := known[srv.Socket]; !ok {
			if values, err = srv.Options(ctx, notify.Option, "@tmag-alert"); err != nil {
				continue
			}
		}
		command, enabled := values[notify.Option], values["@tmag-alert"]
		if command == "" || slices.Contains(commands, command) || !alertNames(enabled, kind) {
			continue
		}
		commands = append(commands, command)
	}
	if len(commands) == 0 {
		return nil // as by default: one tmux call per server, nothing more
	}
	if present(ctx, shown, known) {
		return nil
	}
	a := notify.Alert{Kind: kind, Agent: rec.Agent, Session: rec.ID, State: rec.State(), StateKind: rec.Kind(), Cwd: rec.Cwd}
	if refs := rec.RequestRefs(); kind == "blocked" && len(refs) > 0 {
		q := refs[len(refs)-1] // the prompt that just opened
		a.Request, a.RequestKind, a.Tool = q.ID, q.Kind, q.Tool
	}
	if len(shown) > 0 {
		a.Target = shown[0].Target()
	}
	a.Host, _ = os.Hostname()
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	for _, command := range commands {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := notify.Start(bin, command, a); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
	}
	return nil
}

// present reports whether the person is at a terminal that shows one of the
// panes: its tmux server had their input within notify.Present. A server
// known from Project is not asked again.
func present(ctx context.Context, shown []tmux.Pane, known map[string]attach.Projected) bool {
	seen := map[string]bool{}
	for _, p := range shown {
		if seen[p.Server] {
			continue
		}
		seen[p.Server] = true
		last := known[p.Server].Activity
		if _, ok := known[p.Server]; !ok {
			last, _ = (tmux.Server{Socket: p.Server}).LastInput(ctx)
		}
		if !last.IsZero() && time.Since(last) < notify.Present {
			return true
		}
	}
	return false
}

// maxErrLog bounds hook.log; it is truncated when exceeded.
const maxErrLog = 256 << 10

// RecordError appends a hook failure to <state dir>/hook.log for `tmag
// doctor`, only while the invocation has budget remaining.
func RecordError(parent context.Context, dir, agent string, err error) {
	if err == nil || parent.Err() != nil {
		return
	}
	category := "failed"
	var record *session.RecordError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	case errors.Is(err, context.Canceled):
		category = "canceled"
	case errors.Is(err, notify.ErrLimit):
		category = "limited"
	case errors.As(err, &record):
		category = "records_unavailable"
	}
	recordHookLog(parent, dir, agent, category)
}

// RecordSlow logs a hook that succeeded but took longer than
// SlowAfter. The caller supplies a budget that survives the hook's setup
// deadline: the report belongs in the reserve before the hard cutoff.
func RecordSlow(parent context.Context, dir, agent string) {
	if parent.Err() != nil {
		return
	}
	recordHookLog(parent, dir, agent, "slow")
}

func recordHookLog(parent context.Context, dir, agent, category string) {
	ctx, cancel := context.WithTimeout(parent, 100*time.Millisecond)
	defer cancel()
	if !proc.IsAgent(agent) && agent != "notify" {
		agent = "hook"
	}
	// The line contains fixed categories only, never err.Error(), command output
	// or hook input. Exclusion covers the ceiling check, truncation and append.
	line := []byte(fmt.Sprintf("%s %s %s\n", time.Now().UTC().Format(time.RFC3339), agent, category))
	_ = lockfile.With(ctx, filepath.Join(dir, "hook-log.lock"), func() error {
		f, err := os.OpenFile(filepath.Join(dir, "hook.log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if st.Size()+int64(len(line)) > maxErrLog {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := f.Truncate(0); err != nil {
				return err
			}
		}
		_, err = f.Write(line)
		return err
	})
}
