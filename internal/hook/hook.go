// Package hook handles one agent hook invocation: map the event, bind it to a
// pane, update the pane record and append to the event log.
package hook

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/bind"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
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
		Cwd: p.Cwd, Binding: res.Evidence, Turn: tr.Turn,
	}
	if tr.Action == adapter.Set {
		ev.State, ev.Kind = tr.State, tr.Kind
	}

	if res.Evidence == bind.Unbound {
		if tr.OnlyFrom != "" {
			return nil // cannot check the precondition without a record
		}
		return in.Log.Append(ctx, &ev)
	}

	pane := res.Pane
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
		same := live && cur.Session == p.SessionID && cur.Agent == in.Agent
		if !same && res.Evidence == bind.Session {
			// The record that placed the session here has been replaced since:
			// nothing ties the session to this pane any more.
			ev.Binding, ev.Pane = bind.Unbound, nil
			if tr.OnlyFrom != "" {
				return nil
			}
			return in.Log.Append(ctx, &ev)
		}

		switch tr.Action {
		case adapter.End:
			if same {
				if err := srv.ClearPaneOptions(ctx, pane.ID); err != nil {
					return err
				}
			}
		case adapter.Set:
			if tr.OnlyFrom != "" {
				if !same || cur.State != tr.OnlyFrom {
					return nil
				}
				if tr.Tool != "" && cur.Tool != "" && tr.Tool != cur.Tool {
					return nil // another tool finished; the prompt is still open
				}
			}
			rec := registry.Record{
				Agent: in.Agent, Session: p.SessionID, PID: res.AgentPID,
				State: tr.State, Kind: tr.Kind, Turn: tr.Turn,
				Since:   now().UTC().Truncate(time.Second),
				Binding: res.Evidence, Command: fresh.Command,
			}
			if tr.State == adapter.Blocked {
				rec.Tool = tr.Tool
			}
			prev := ""
			if same {
				prev = cur.State
				if rec.Turn == "" {
					rec.Turn = cur.Turn
				}
				if cur.State == rec.State {
					rec.Since = cur.Since
				}
				// Only the start of a turn confirms a submitted prompt.
				if rec.State == adapter.Idle {
					rec.Sent = cur.Sent
				}
				if rec.Equal(cur) {
					return nil // nothing changed
				}
			}
			if err := registry.Write(ctx, srv, pane.ID, rec); err != nil {
				return err
			}
			ev.Turn = rec.Turn
			ring = Alert(prev, rec.State)
		}
		return in.Log.Append(ctx, &ev)
	})
	if err != nil {
		return err
	}
	if tr.Action == adapter.Set {
		releaseOthers(ctx, w, pane, in.Agent, p.SessionID)
	}
	alert(ctx, tmux.Server{Socket: pane.Server}, pane, ring)
	return nil
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

// releaseOthers clears records of the same session left on other panes, e.g.
// after a Codex thread moved to another terminal.
func releaseOthers(ctx context.Context, w *bind.World, keep tmux.Pane, agent, session string) {
	for _, p := range w.Panes {
		if p.Server == keep.Server && p.ID == keep.ID {
			continue
		}
		rec := registry.FromPane(p)
		if rec.Session == session && rec.Agent == agent {
			_ = tmux.Server{Socket: p.Server}.ClearPaneOptions(ctx, p.ID)
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
