// Package hook handles one agent hook invocation: map the event, apply it to
// the session, attach the session to the pane it runs in when the hook
// proves one, update the panes that show it, alert the person, and log what
// changed.
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

// loadProcesses reads the process table into w, and loadPanes lists the
// panes. Tests replace them to stage a world between the hook's snapshots.
var (
	loadProcesses = func(ctx context.Context, w *attach.World) error { return w.LoadProcesses(ctx) }
	loadPanes     = func(ctx context.Context, w *attach.World) error { return w.LoadPanes(ctx) }
)

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
}

// Run processes the hook within Deadline. It never writes to stdout: agents
// inject hook stdout into the model context.
func Run(in Input) error {
	return RunContext(context.Background(), in)
}

// RunContext processes a hook using the earlier of the parent's budget and
// Deadline, including input. Payload readers must cooperate with cancellation;
// the CLI owns its inherited input with contextio.Own. No read worker is left
// behind if an injected reader does not support cancellation.
func RunContext(parent context.Context, in Input) (err error) {
	ctx, cancel := context.WithTimeout(parent, Deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !proc.IsAgent(in.Agent) {
		return fmt.Errorf("unknown agent %q", in.Agent)
	}
	data, err := contextio.ReadAll(ctx, in.Payload, maxPayload)
	if err != nil {
		return err
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
		if !tr.Waits {
			tr.Request = session.NewRequestID()
		}
	}
	if !session.ValidID(p.SessionID) {
		return fmt.Errorf("unusable session id %q", p.SessionID)
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}

	// Unlike attach.Load, the hook reads the process table before it lists
	// panes, so that the session's record and event are saved before any tmux
	// server can hold the hook up. The hook's own proof stays fresh: Attach
	// re-reads the pane under its lock and snapshots the process table again
	// before it writes an attachment.
	w := &attach.World{}
	hostKnown := false
	var host proc.Host
	if err := loadProcesses(ctx, w); err != nil {
		// An unreadable process table proves no host and no inbox. Leave
		// the record's host and inbox as they were and record the state.
		w = &attach.World{}
	} else {
		host, hostKnown = w.Proc.HostOf(in.PID)
		if hostKnown && p.Mode != "" && p.Mode != "tui" {
			// Pi and oh-my-pi say when they run for a program rather than a
			// person, which their process does not show: such a session runs as
			// a service, which no pane shows and no channel controls. The claim
			// only takes away, so it needs no proof.
			host.Role = proc.Service
		}
	}
	env := attach.Env{Log: in.Log, Store: session.Store{Dir: in.Log.Dir}}
	contact := now().UTC()
	ev := eventlog.Event{Agent: in.Agent, Session: p.SessionID, Event: p.HookEventName, Cwd: p.Cwd, LastHookAt: contact}
	// The inbox a hook's agent names, and the process that serves it, are
	// recorded for a send to prove later. The hook never connects to it.
	inbox := inboxFor(w, in.PID, in.Agent, inboxOf(in.Agent, p, os.Getenv))

	// A record that cannot be read is the hook's to replace: the session's
	// state comes from its hooks, and this one reports it anew.
	get := func() (session.Record, bool, error) {
		rec, found, err := env.Store.Get(ctx, in.Agent, p.SessionID)
		var fault *session.RecordError
		if errors.As(err, &fault) {
			RecordErrorContext(ctx, in.Log.Dir, in.Agent, err)
			return session.Record{}, false, nil
		}
		return rec, found, err
	}

	var prev, rec session.Record
	wrote, ended := false, false
	if tr.Action == adapter.End {
		// Whatever the agent calls it, the contract's end of a session is
		// SessionEnd, which followers drop the session on.
		ev.Event = eventlog.End
		err := env.Store.Lock(ctx, in.Agent, p.SessionID, func() error {
			cur, found, err := get()
			if err != nil {
				return err
			}
			if !found {
				cur = session.Record{Agent: in.Agent, ID: p.SessionID}
			}
			return env.Store.End(ctx, in.Log, cur, ev)
		})
		if err != nil {
			return err
		}
		ended = true
	} else {
		err = env.Store.Lock(ctx, in.Agent, p.SessionID, func() error {
			cur, found, err := get()
			if err != nil {
				return err
			}
			if !found {
				cur = session.Record{Agent: in.Agent, ID: p.SessionID}
			}
			base := cur
			if p.Cwd != "" {
				base.Cwd = p.Cwd
			}
			if hostKnown {
				base.Host = session.Host{PID: host.Process.PID, Start: host.Process.Start, Role: host.Role}
			}
			if p.TranscriptPath != "" {
				base.Transcript = p.TranscriptPath
			}
			if inbox != nil {
				base.Inbox = inbox
			}
			next, changed := session.Apply(base, tr, now())
			recorded := moved(cur, base)
			if found && !changed && !recorded && contact.Sub(cur.LastHookAt) < ContactInterval {
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
		if err != nil || rec.ID == "" {
			return err
		}
	}

	// The record and its event are in place. Pane work comes after them and
	// cannot undo them: an error still reaches hook.log through the caller, but
	// leaves the recorded state as the agent reported it.
	if err := loadPanes(ctx, w); err != nil {
		return err
	}
	if w.Proc != nil {
		// Housekeeping comes last, on a process table read after the panes as
		// attach.Load reads them: a table older than a pane could miss a client
		// that attached meanwhile, and reconciliation would detach it. Without
		// fresh complete evidence there is nothing to reconcile against.
		defer func() {
			if loadErr := loadProcesses(ctx, w); loadErr != nil {
				if ctx.Err() != nil {
					err = errors.Join(err, ctx.Err())
				}
				return
			}
			if !w.Proc.Complete() {
				return
			}
			err = errors.Join(err, attach.Reconcile(ctx, env, w))
		}()
	}
	if ended {
		return env.DetachAll(ctx, w, in.Agent, p.SessionID)
	}
	if !wrote {
		// Nothing changed, but the hook still proves its pane shows the
		// session: an earlier hook may have died before attaching it.
		if c, ok := w.ClientOf(host); hostKnown && ok && !c.Shows(rec) {
			return env.Attach(ctx, w, c, rec, attach.Process)
		}
		return nil
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

	ring := Alert(prev.State(), rec.State())
	var shown []tmux.Pane
	if c, ok := w.ClientOf(host); hostKnown && ok {
		if err := env.Attach(ctx, w, c, rec, attach.Process); err != nil {
			return err
		}
		shown = append(shown, c.Pane)
	}
	if w.Proc != nil {
		for _, s := range w.Attached(rec) {
			if len(shown) > 0 && s.Client.Pane.Server == shown[0].Server && s.Client.Pane.ID == shown[0].ID {
				continue // just attached, projection included
			}
			if err := env.Show(ctx, s.Client.Pane, s.Attachment); err != nil {
				return err
			}
			shown = append(shown, s.Client.Pane)
		}
	}
	for _, pane := range shown {
		alert(ctx, tmux.Server{Socket: pane.Server}, pane, ring)
	}
	return tell(ctx, rec, shown, ring)
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
	if kind != "" && !pane.Watched && alerts(ctx, srv, kind) {
		_ = pane.Bell()
	}
}

// alerts reports whether the server's @tmag-alert names this kind of alert.
func alerts(ctx context.Context, srv tmux.Server, kind string) bool {
	for _, enabled := range strings.Split(srv.GlobalOption(ctx, "@tmag-alert"), ",") {
		if strings.TrimSpace(enabled) == kind {
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
// wait for the network.
func tell(ctx context.Context, rec session.Record, shown []tmux.Pane, kind string) error {
	if kind == "" {
		return nil
	}
	servers, err := tmux.ServersContext(ctx)
	if err != nil {
		return err
	}
	var commands []string
	for _, srv := range servers {
		command := srv.GlobalOption(ctx, notify.Option)
		if command == "" || slices.Contains(commands, command) || !alerts(ctx, srv, kind) {
			continue
		}
		commands = append(commands, command)
	}
	if len(commands) == 0 {
		return nil // as by default: one tmux call per server, nothing more
	}
	if present(ctx, shown) {
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
// panes: its tmux server had their input within notify.Present.
func present(ctx context.Context, shown []tmux.Pane) bool {
	seen := map[string]bool{}
	for _, p := range shown {
		if seen[p.Server] {
			continue
		}
		seen[p.Server] = true
		if last, ok := (tmux.Server{Socket: p.Server}).LastInput(ctx); ok && time.Since(last) < notify.Present {
			return true
		}
	}
	return false
}

// maxErrLog bounds hook.log; it is truncated when exceeded.
const maxErrLog = 256 << 10

// RecordError appends a hook failure to <state dir>/hook.log for `tmag doctor`.
func RecordError(dir, agent string, err error) {
	RecordErrorContext(context.Background(), dir, agent, err)
}

// RecordErrorContext logs only while the invocation has budget remaining.
func RecordErrorContext(parent context.Context, dir, agent string, err error) {
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

// RecordSlowContext logs a hook that succeeded but took longer than
// SlowAfter. The caller supplies a budget that survives the hook's setup
// deadline: the report belongs in the reserve before the hard cutoff.
func RecordSlowContext(parent context.Context, dir, agent string) {
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
		return contextio.WriteAll(ctx, f, line)
	})
}
