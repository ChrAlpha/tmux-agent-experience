// Package hook handles one agent hook invocation: map the event, apply it to
// the session, attach the session to the pane it runs in when the hook
// proves one, update the panes that show it, alert the person, and log what
// changed.
package hook

import (
	"context"
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
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
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
	if in.Agent != proc.Claude && in.Agent != proc.Codex {
		return fmt.Errorf("unknown agent %q", in.Agent)
	}
	data, err := io.ReadAll(io.LimitReader(in.Payload, maxPayload))
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
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()

	w, err := attach.Load(ctx)
	if err != nil {
		return err
	}
	env := attach.Env{Log: in.Log, Store: session.Store{Dir: in.Log.Dir}}
	// Housekeeping on the world as loaded, once the hook's own work is done.
	defer attach.Reconcile(ctx, env, w)

	host, hostKnown := w.Proc.HostOf(in.PID)
	ev := eventlog.Event{Agent: in.Agent, Session: p.SessionID, Event: p.HookEventName, Cwd: p.Cwd}

	get := func() (session.Record, bool, error) { return env.Store.Current(ctx, in.Log, in.Agent, p.SessionID) }

	if tr.Action == adapter.End {
		err := env.Store.Lock(ctx, in.Agent, p.SessionID, func() error {
			cur, found, err := get()
			if err != nil {
				return err
			}
			if !found {
				return in.Log.Append(ctx, &ev)
			}
			return env.Store.End(ctx, in.Log, cur, ev)
		})
		if err != nil {
			return err
		}
		return env.DetachAll(ctx, w, in.Agent, p.SessionID)
	}

	var prev, rec session.Record
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
		next, changed := session.Apply(base, tr, now())
		if found && !changed && base.Cwd == cur.Cwd && base.Host == cur.Host && base.Transcript == cur.Transcript {
			return nil
		}
		// Calls and other background work change without an event; whatever
		// an event reports of the session (session.Report) is logged.
		saved, err := env.Store.Save(ctx, in.Log, next, ev)
		if err != nil {
			return err
		}
		prev, rec = cur, saved
		return nil
	})
	if err != nil || rec.ID == "" {
		return err
	}
	// An answer hook whose prompt closed (answered in the terminal, the call
	// ran, the turn ended) would otherwise wait on: the agent ignores what it
	// prints now.
	for _, q := range prev.Requests {
		if _, open := rec.Request(q.ID); !open && q.Hook {
			answer.Close(ctx, in.Log.Dir, q.ID)
		}
	}

	ring := Alert(prev.State(), rec.State())
	var shown []tmux.Pane
	if c, ok := w.ClientOf(host); hostKnown && ok {
		if err := env.Attach(ctx, w, c, rec, attach.Process); err != nil {
			return err
		}
		shown = append(shown, c.Pane)
	}
	for _, s := range w.Attached(rec) {
		if len(shown) > 0 && s.Client.Pane.Server == shown[0].Server && s.Client.Pane.ID == shown[0].ID {
			continue // just attached, projection included
		}
		if err := env.Show(ctx, s.Client.Pane, s.Attachment); err != nil {
			return err
		}
		shown = append(shown, s.Client.Pane)
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
	servers := tmux.Servers()
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
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "hook.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, serr := os.Stat(path); serr == nil && st.Size() > maxErrLog {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, ferr := os.OpenFile(path, flags, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %v\n", time.Now().UTC().Format(time.RFC3339), agent, err)
}
