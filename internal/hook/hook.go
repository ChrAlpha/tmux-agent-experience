// Package hook handles one agent hook invocation: map the event, bind it to a
// pane, update the pane record and append to the event log.
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
	"github.com/ChrAlpha/tmux-agent-experience/internal/bind"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/registry"
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
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()

	w, err := bind.Load(ctx)
	if err != nil {
		return err
	}
	res := bind.Resolve(w, bind.Request{Agent: in.Agent, SessionID: p.SessionID, Cwd: p.Cwd, PID: in.PID})

	ev := eventlog.Event{
		Agent: in.Agent, Session: p.SessionID, Event: p.HookEventName,
		Cwd: p.Cwd, Binding: res.Evidence, Turn: tr.Turn, State: tr.State, Kind: tr.Kind,
	}

	// Housekeeping on the world as loaded, once the hook's own work is done:
	// records of agents gone from their pane, which a new agent's hooks clear
	// even before they can bind, and once the session is placed, its records
	// on panes it has left.
	var moved func(tmux.Pane, registry.Record) bool
	defer func() { query.Prune(ctx, in.Log.Dir, w, moved) }()

	if res.Evidence == bind.Unbound {
		if tr.NeedsRecord() {
			return nil // what it changes depends on a record there is none of
		}
		return in.Log.Append(ctx, &ev)
	}

	pane := res.Pane
	moved = func(o tmux.Pane, rec registry.Record) bool {
		return (o.Server != pane.Server || o.ID != pane.ID) && rec.Agent == in.Agent && rec.Session == p.SessionID
	}
	ev.Pane = &eventlog.PaneRef{Server: pane.Server, ID: pane.ID}
	ring := ""
	err = registry.Lock(ctx, in.Log.Dir, pane.Server, pane.ID, func() error {
		// Decide on the record as it is now, not as it was when the world was
		// loaded: a sender or another hook may have changed it since.
		srv := tmux.Server{Socket: pane.Server}
		fresh, err := srv.Pane(ctx, pane.ID)
		if err != nil {
			return err
		}
		cur, live := w.Record(fresh)
		if !live || cur.Session != p.SessionID || cur.Agent != in.Agent {
			if res.Evidence == bind.Session {
				// The record that placed the session here has been replaced
				// since: nothing ties the session to this pane any more.
				ev.Binding, ev.Pane = bind.Unbound, nil
				if tr.NeedsRecord() {
					return nil
				}
				return in.Log.Append(ctx, &ev)
			}
			cur = registry.Record{}
		}
		if tr.Action == adapter.End {
			if cur.Bound() {
				if err := srv.ClearPaneOptions(ctx, pane.ID); err != nil {
					return err
				}
			}
			return in.Log.Append(ctx, &ev)
		}
		rec, changed := next(cur, tr, registry.Record{
			Agent: in.Agent, Session: p.SessionID, PID: res.AgentPID,
			Since:   now().UTC().Truncate(time.Second),
			Binding: res.Evidence, Command: fresh.Command,
		})
		if !changed {
			return nil
		}
		if err := registry.Write(ctx, srv, pane.ID, rec); err != nil {
			return err
		}
		if reported(rec).Equal(reported(cur)) {
			return nil
		}
		ev.State, ev.Kind, ev.Turn = rec.State, rec.Kind, rec.Turn
		ring = Alert(cur.State, rec.State)
		return in.Log.Append(ctx, &ev)
	})
	if err != nil {
		return err
	}
	alert(ctx, tmux.Server{Socket: pane.Server}, pane, ring)
	return nil
}

// next applies tr to cur, the session's current record in the pane (zero when
// the pane holds none), and reports whether anything changed. id carries the
// record's identity and the time of the event.
//
// Open prompts block the agent. A prompt closes only on evidence that the
// user dealt with it: the tool call it may be about is over, the agent's
// batch of calls resolved or the agent stopped, or the turn it belongs to
// ended. Once the last one closes, the agent works on as it did before.
func next(cur registry.Record, tr adapter.Transition, id registry.Record) (registry.Record, bool) {
	if tr.NeedsRecord() && !cur.Bound() {
		return cur, false
	}
	rec := id
	rec.State, rec.Kind, rec.Base, rec.Turn = cur.State, cur.Kind, cur.Base, cur.Turn
	rec.Pending, rec.Calls = slices.Clone(cur.Pending), slices.Clone(cur.Calls)
	if tr.Turn != "" {
		rec.Turn = tr.Turn
	}
	switch tr.Action {
	case adapter.Set:
		rec.Pending = slices.DeleteFunc(rec.Pending, func(p adapter.Prompt) bool { return !tr.Keep.Has(p.Agent) })
		rec.Calls = slices.DeleteFunc(rec.Calls, func(c adapter.Call) bool { return !tr.Keep.Has(c.Agent) })
		rec.State, rec.Kind, rec.Base, rec.Turn = tr.State, tr.Kind, "", tr.Turn
	case adapter.Ask:
		// A reminder cannot be told apart from a prompt already pending, and
		// outside a turn it cannot be about one.
		if tr.Reminder && (len(cur.Pending) > 0 || cur.State != adapter.Working) {
			return cur, false
		}
		rec.Calls = startCall(rec.Calls, tr.Call)
		if !slices.Contains(rec.Pending, tr.Prompt) {
			rec.Pending = append(rec.Pending, tr.Prompt)
		}
	case adapter.Start:
		rec.Calls = startCall(rec.Calls, tr.Call)
	case adapter.Finish:
		c := tr.Call
		rec.Calls = slices.DeleteFunc(rec.Calls, func(o adapter.Call) bool { return o.ID == c.ID })
		running := slices.ContainsFunc(rec.Calls, func(o adapter.Call) bool { return o.Agent == c.Agent && o.Tool == c.Tool })
		if i := slices.Index(rec.Pending, adapter.Prompt{Agent: c.Agent, Tool: c.Tool}); i >= 0 && !running {
			rec.Pending = slices.Delete(rec.Pending, i, i+1)
		} else {
			rec.Pending = answerUnnamed(rec.Pending)
		}
	case adapter.Settle:
		agent := tr.Call.Agent
		rec.Calls = slices.DeleteFunc(rec.Calls, func(o adapter.Call) bool { return o.Agent == agent })
		rec.Pending = slices.DeleteFunc(rec.Pending, func(p adapter.Prompt) bool { return p.Agent == agent || p.Tool == "" })
	case adapter.Answer:
		if i := slices.Index(rec.Pending, tr.Prompt); i >= 0 {
			rec.Pending = slices.Delete(rec.Pending, i, i+1)
		} else {
			rec.Pending = answerUnnamed(rec.Pending)
		}
	default:
		return cur, false
	}
	switch {
	case len(rec.Pending) > 0:
		if rec.State != adapter.Blocked {
			rec.Base = underlying(rec.State, rec.Kind)
		}
		rec.State, rec.Kind = adapter.Blocked, rec.Pending[0].Kind()
	case rec.State == adapter.Blocked:
		rec.State, rec.Kind, rec.Base = adapter.Working, rec.Base, ""
	}
	if len(rec.Pending) == 0 {
		rec.Pending = nil
	}
	if len(rec.Calls) == 0 {
		rec.Calls = nil
	}
	if !cur.Bound() {
		return rec, true
	}
	if rec.Turn == "" {
		rec.Turn = cur.Turn
	}
	if rec.State == cur.State {
		rec.Since = cur.Since
	}
	// Only the start of a turn confirms a submitted prompt.
	if rec.State == adapter.Idle {
		rec.Sent = cur.Sent
	}
	return rec, !rec.Equal(cur)
}

func startCall(calls []adapter.Call, c adapter.Call) []adapter.Call {
	if c.ID == "" || slices.ContainsFunc(calls, func(o adapter.Call) bool { return o.ID == c.ID }) {
		return calls
	}
	return append(calls, c)
}

// answerUnnamed closes a prompt that named no tool. It says nothing about the
// tool, and a reminder does not reliably name the agent that asked: any
// finished tool answers it.
func answerUnnamed(pending []adapter.Prompt) []adapter.Prompt {
	if i := slices.IndexFunc(pending, func(p adapter.Prompt) bool { return p.Tool == "" }); i >= 0 {
		return slices.Delete(pending, i, i+1)
	}
	return pending
}

// underlying is the kind of work an agent blocked in the given state returns
// to: its turn, or once the main agent's turn is over, background work.
func underlying(state, kind string) string {
	if state == adapter.Idle || kind == adapter.KindBackground {
		return adapter.KindBackground
	}
	return ""
}

// reported strips what events do not report, the bookkeeping of tool calls.
func reported(r registry.Record) registry.Record {
	r.Calls = nil
	return r
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
	for _, enabled := range strings.Split(srv.GlobalOption(ctx, "@tmag-alert"), ",") {
		if strings.TrimSpace(enabled) == kind {
			_ = pane.Bell()
			return
		}
	}
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
