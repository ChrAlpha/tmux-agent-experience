// Package hook handles one agent hook invocation: map the event, apply it to
// the session, attach the session to the pane it runs in when the hook
// proves one, update the panes that show it, log the change and any alert,
// and tell the person.
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
func RunContext(parent context.Context, in Input) error {
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

	w, err := attach.Load(ctx)
	if err != nil {
		return err
	}
	env := attach.Env{Log: in.Log, Store: session.Store{Dir: in.Log.Dir}}
	// Housekeeping on the world as loaded, once the hook's own work is done.
	defer func() {
		if err := attach.Reconcile(ctx, env, w); err != nil {
			RecordErrorContext(ctx, in.Log.Dir, in.Agent, err)
		}
	}()

	host, hostKnown := w.Proc.HostOf(in.PID)
	if hostKnown && p.Mode != "" && p.Mode != "tui" {
		// Pi and oh-my-pi say when they run for a program rather than a
		// person, which their process does not show: such a session runs as
		// a service, which no pane shows and no channel controls. The claim
		// only takes away, so it needs no proof.
		host.Role = proc.Service
	}
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
		return env.DetachAll(ctx, w, in.Agent, p.SessionID)
	}

	var prev, rec session.Record
	var shown []tmux.Pane
	var own *attach.Client
	var others []attach.Shown
	var ring string
	var atTerminal bool
	var opts optionCache
	wrote := false
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
		ring = Alert(cur.State(), next.State())
		own, others, shown = sightOf(w, host, hostKnown, next)
		// Presence is what the bell and @tmag-notify rest on, what an Alert
		// event reports, and what a seen turn rests on: computed once, for a
		// change that alerts, and no tmux call for one that does not.
		if ring != "" {
			present := presence(shown, func(s tmux.Server) (time.Time, bool) { return s.LastInput(ctx) })
			for _, at := range present {
				atTerminal = atTerminal || at
			}
			if ring == "finished" && next.Finished() && seenAt(shown, present, &opts, ctx) {
				next.Seen = next.Turn
			}
		}
		// Calls and other background work change without an event; whatever
		// an event reports of the session (session.Report) is logged.
		if err := env.Store.Save(ctx, in.Log, cur, next, ev); err != nil {
			return err
		}
		if ring != "" {
			// Seen is logged after the change and its Alert, under the same
			// lock, and written with the change.
			if err := logAlert(ctx, in.Log, next, ring, atTerminal); err != nil {
				RecordErrorContext(ctx, in.Log.Dir, in.Agent, err)
			}
			if next.Seen != cur.Seen {
				seen := eventlog.Event{Event: eventlog.Seen, Agent: next.Agent, Session: next.ID, Turn: next.Seen}
				if err := in.Log.Append(ctx, &seen); err != nil {
					RecordErrorContext(ctx, in.Log.Dir, in.Agent, err)
				}
			}
		}
		prev, rec, wrote = cur, next, true
		return nil
	})
	if err != nil || rec.ID == "" {
		return err
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

	if own != nil {
		if err := env.Attach(ctx, w, *own, rec, attach.Process); err != nil {
			return err
		}
	}
	for _, s := range others {
		if err := env.Show(ctx, s.Client.Pane, s.Attachment); err != nil {
			return err
		}
	}
	for _, pane := range shown {
		alert(ctx, tmux.Server{Socket: pane.Server}, pane, ring, &opts)
	}
	return tell(ctx, rec, shown, ring, atTerminal, &opts)
}

// sightOf is the panes that show rec: the hook's own client, whose process
// ancestry proves the pane, then every stored attachment. own is the pane
// the hook attaches; its projection comes with the attachment.
func sightOf(w *attach.World, host proc.Host, hostKnown bool, rec session.Record) (own *attach.Client, others []attach.Shown, shown []tmux.Pane) {
	if c, ok := w.ClientOf(host); hostKnown && ok {
		own = &c
		shown = append(shown, c.Pane)
	}
	for _, s := range w.Attached(rec) {
		if own != nil && s.Client.Pane.Server == own.Pane.Server && s.Client.Pane.ID == own.Pane.ID {
			continue
		}
		others = append(others, s)
		shown = append(shown, s.Client.Pane)
	}
	return own, others, shown
}

// seenAt reports whether the person sees rec on the terminal: a watched pane
// shows it, its server had the person's input within notify.Present, and the
// server learns seen turns from the terminal (@tmag-seen).
func seenAt(shown []tmux.Pane, present map[string]bool, opts *optionCache, ctx context.Context) bool {
	for _, p := range shown {
		if !p.Watched || !present[p.Server] {
			continue
		}
		if opts.get(ctx, tmux.Server{Socket: p.Server}).seen == "off" {
			continue
		}
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

// logAlert appends the Alert event for a change that alerts, after the event
// that reports the change. The alert holds no state: a client that applies it
// to a snapshot discards one the current state contradicts.
func logAlert(ctx context.Context, log *eventlog.Log, rec session.Record, kind string, atTerminal bool) error {
	a := eventlog.AlertInfo{Kind: kind, Present: atTerminal}
	if kind == "finished" {
		a.Turn = rec.Turn
	}
	if kind == "blocked" {
		if q, ok := blockedRequest(rec); ok {
			a.Request = &q
		}
	}
	return log.Append(ctx, &eventlog.Event{Agent: rec.Agent, Session: rec.ID, Event: eventlog.Alert, Alert: &a})
}

// blockedRequest names the request a blocked alert reports: the prompt that
// just opened, when the session waits on one.
func blockedRequest(rec session.Record) (eventlog.AlertRequest, bool) {
	refs := rec.RequestRefs()
	if len(refs) == 0 {
		return eventlog.AlertRequest{}, false
	}
	q := refs[len(refs)-1]
	return eventlog.AlertRequest{ID: q.ID, Kind: q.Kind, Tool: q.Tool}, true
}

// alert rings the pane's bell when the tmux plugin enabled this kind of alert
// (@tmag-alert) and nobody is looking at the pane. tmux turns the bell into the
// window's alert flag, which prefix + M-n jumps to.
func alert(ctx context.Context, srv tmux.Server, pane tmux.Pane, kind string, opts *optionCache) {
	if kind != "" && !pane.Watched && opts.alerts(ctx, srv, kind) {
		_ = pane.Bell()
	}
}

// optionCache reads the plugin's options from tmux once per server, in one
// call: a hook that needs @tmag-seen has @tmag-alert too.
type optionCache struct{ values map[string]serverOptions }

// serverOptions are the plugin's options on one server. An unreadable or
// unset option is "", which means the default.
type serverOptions struct{ alert, seen string }

func (o *optionCache) get(ctx context.Context, srv tmux.Server) serverOptions {
	if o.values == nil {
		o.values = map[string]serverOptions{}
	}
	if v, ok := o.values[srv.Socket]; ok {
		return v
	}
	v := serverOptions{}
	if values, err := srv.GlobalOptionsContext(ctx, "@tmag-alert", tmux.OptionSeen); err == nil {
		v.alert, v.seen = values["@tmag-alert"], values[tmux.OptionSeen]
	}
	o.values[srv.Socket] = v
	return v
}

// alerts reports whether the server's @tmag-alert names this kind of alert.
func (o *optionCache) alerts(ctx context.Context, srv tmux.Server, kind string) bool {
	for _, enabled := range strings.Split(o.get(ctx, srv).alert, ",") {
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
// wait for the network. atTerminal is what the alert event reported.
func tell(ctx context.Context, rec session.Record, shown []tmux.Pane, kind string, atTerminal bool, opts *optionCache) error {
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
		if command == "" || slices.Contains(commands, command) || !opts.alerts(ctx, srv, kind) {
			continue
		}
		commands = append(commands, command)
	}
	if len(commands) == 0 {
		return nil // as by default: one tmux call per server, nothing more
	}
	if atTerminal {
		return nil
	}
	a := notify.Alert{Kind: kind, Agent: rec.Agent, Session: rec.ID, State: rec.State(), StateKind: rec.Kind(), Cwd: rec.Cwd}
	if kind == "blocked" {
		if q, ok := blockedRequest(rec); ok {
			a.Request, a.RequestKind, a.Tool = q.ID, q.Kind, q.Tool
		}
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

// presence reports, per server that shows one of the panes, whether a client
// had the person's input within notify.Present. It asks each server once.
func presence(shown []tmux.Pane, lastInput func(tmux.Server) (time.Time, bool)) map[string]bool {
	at := map[string]bool{}
	for _, p := range shown {
		if _, asked := at[p.Server]; asked {
			continue
		}
		last, ok := lastInput(tmux.Server{Socket: p.Server})
		at[p.Server] = ok && time.Since(last) < notify.Present
	}
	return at
}

// present reports whether the person is at a terminal that shows one of the
// panes: one of their tmux servers had their input within notify.Present.
func present(shown []tmux.Pane, lastInput func(tmux.Server) (time.Time, bool)) bool {
	for _, at := range presence(shown, lastInput) {
		if at {
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
	ctx, cancel := context.WithTimeout(parent, 100*time.Millisecond)
	defer cancel()
	if !proc.IsAgent(agent) && agent != "notify" {
		agent = "hook"
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
