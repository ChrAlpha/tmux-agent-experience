package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/bind"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// cmdHook always exits 0 and never writes stdout: a broken tmag must not
// disturb the agent. Failures go to <state dir>/hook.log.
func (a *app) cmdHook(args []string) (code int) {
	if len(args) != 1 || !query.IsAgentKind(args[0]) {
		fmt.Fprintln(a.stderr, "usage: tmag hook <claude|codex>")
		return ExitOK
	}
	defer func() {
		if r := recover(); r != nil {
			hook.RecordError(a.log.Dir, args[0], fmt.Errorf("panic: %v", r))
			code = ExitOK
		}
	}()
	err := hook.Run(hook.Input{Agent: args[0], Payload: a.stdin, Log: a.log, PID: os.Getpid()})
	if err != nil {
		hook.RecordError(a.log.Dir, args[0], err)
	}
	return ExitOK
}

func (a *app) cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err != nil {
		return a.usageError("list", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	snap, err := query.TakeSnapshot(ctx, a.log)
	if err != nil {
		return a.fail("list", err)
	}
	if a.json {
		a.emit(map[string]any{"seq": snap.Seq, "agents": snap.Agents})
		return ExitOK
	}
	a.printAgents(snap.Agents)
	return ExitOK
}

func (a *app) printAgents(agents []query.Agent) {
	if len(agents) == 0 {
		fmt.Fprintln(a.stdout, "no agents")
		return
	}
	servers := map[string]bool{}
	for _, ag := range agents {
		servers[ag.Pane.Server] = true
	}
	home, _ := os.UserHomeDir()
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tPANE\tAGENT\tSTATE\tFOR\tID\tCWD")
	for _, ag := range agents {
		target := ag.Pane.Target
		if len(servers) > 1 {
			target = filepath.Base(ag.Pane.Server) + "/" + target
		}
		state := ag.State
		if ag.Kind != "" {
			state += " (" + ag.Kind + ")"
		}
		id := shortID(ag.ID)
		if ag.Binding == bind.Unbound {
			id = "-"
		}
		cwd := ag.Pane.Cwd
		if home != "" && strings.HasPrefix(cwd, home) {
			cwd = "~" + strings.TrimPrefix(cwd, home)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", target, ag.Pane.ID, ag.Agent, state, age(ag.Since), id, cwd)
	}
	tw.Flush()
}

func age(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Since(*t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (a *app) resolve(ctx context.Context, target string) (query.Agent, error) {
	w, err := bind.Load(ctx)
	if err != nil {
		return query.Agent{}, err
	}
	return query.Resolve(query.Agents(w), target)
}

func (a *app) cmdInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	n := fs.Int("events", 20, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = errors.New("expected one target")
	}
	if err != nil {
		return a.usageError("inspect", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	ag, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.fail("inspect", err)
	}
	var events []eventlog.Event
	if ag.ID != "" {
		if events, err = query.SessionEvents(ctx, a.log, ag.ID, *n); err != nil {
			return a.fail("inspect", err)
		}
	}
	if events == nil {
		events = []eventlog.Event{}
	}
	if a.json {
		a.emit(map[string]any{"agent": ag, "events": events})
		return ExitOK
	}
	a.printAgents([]query.Agent{ag})
	if ag.ID != "" {
		fmt.Fprintf(a.stdout, "\nid: %s\nbinding: %s\n", ag.ID, ag.Binding)
	}
	for _, e := range events {
		state := e.State
		if e.Kind != "" {
			state += " (" + e.Kind + ")"
		}
		fmt.Fprintf(a.stdout, "%6d  %s  %-18s %s\n", e.Seq, e.Time.Local().Format("15:04:05"), e.Event, state)
	}
	return ExitOK
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

func (a *app) cmdWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	since := fs.Int64("since", -1, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err != nil {
		return a.usageError("watch", err)
	}
	a.json = true
	ctx, cancel := signalContext()
	defer cancel()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	out := bufio.NewWriter(a.stdout)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	write := func(doc map[string]any) error {
		doc["schema"] = version.Schema
		if err := enc.Encode(doc); err != nil {
			return err
		}
		return out.Flush()
	}
	writeSnapshot := func(seq int64, agents []query.Agent) error {
		if agents == nil {
			agents = []query.Agent{}
		}
		return write(map[string]any{"type": "snapshot", "seq": seq, "agents": agents})
	}

	// mu orders output: events, and snapshots taken between them.
	var (
		mu     sync.Mutex
		last   int64  // seq of the last event written, or of the snapshot
		roster string // the agent set the client was last shown; "" when unknown
	)
	snapshot := func() error {
		snap, err := query.TakeSnapshot(ctx, a.log)
		if err != nil {
			return err
		}
		last, roster = snap.Seq, rosterOf(snap.Agents)
		return writeSnapshot(snap.Seq, snap.Agents)
	}
	if *since >= 0 {
		events, complete, err := a.log.Since(ctx, *since)
		if err != nil {
			return a.fail("watch", err)
		}
		if complete {
			last = *since
			for _, e := range events {
				if err := write(map[string]any{"type": "event", "event": e}); err != nil {
					return ExitOK
				}
				last = e.Seq
			}
		} else if err = snapshot(); err != nil {
			return a.fail("watch", err)
		}
	} else if err = snapshot(); err != nil {
		return a.fail("watch", err)
	}

	// Agents also appear, vanish or lose their binding without an event: a
	// pane closes, an agent is killed, a new one has not reported yet. A fresh
	// snapshot reports such changes. It is listed under mu, after every event
	// written so far, whose records were all written before their events.
	go func() {
		tick := time.NewTicker(watchRescan)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			mu.Lock()
			if w, err := bind.Load(ctx); err == nil {
				if agents := query.Agents(w); rosterOf(agents) != roster {
					roster = rosterOf(agents)
					if err := writeSnapshot(last, agents); err != nil {
						stop(err)
					}
				}
			}
			mu.Unlock()
		}
	}()

	err = a.log.Follow(ctx, last, 200*time.Millisecond, func(e eventlog.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if err := write(map[string]any{"type": "event", "event": e}); err != nil {
			return err
		}
		last = e.Seq
		return nil
	})
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	if err != nil && !errors.Is(err, context.Canceled) && !isClosedPipe(err) {
		return a.fail("watch", err)
	}
	return ExitOK
}

// watchRescan is how often watch looks for agents that changed without an event.
const watchRescan = 2 * time.Second

// rosterOf identifies the set of agents and their bindings, but not their
// states, which events report.
func rosterOf(agents []query.Agent) string {
	var b strings.Builder
	for _, ag := range agents {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s\x00%s\n", ag.Pane.Server, ag.Pane.ID, ag.Agent, ag.ID, ag.Binding)
	}
	return b.String() + "."
}

func isClosedPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}

func (a *app) cmdRead(args []string) int {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	lines := fs.Int("lines", 100, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = errors.New("expected one target")
	}
	if err == nil && *lines <= 0 {
		err = errors.New("--lines must be positive")
	}
	if err != nil {
		return a.usageError("read", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	ag, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.fail("read", err)
	}
	text, err := tmux.Server{Socket: ag.Pane.Server}.Capture(ctx, ag.Pane.ID, *lines)
	if err != nil {
		return a.fail("read", err)
	}
	if a.json {
		a.emit(map[string]any{"agent": ag, "lines": *lines, "text": text})
		return ExitOK
	}
	fmt.Fprint(a.stdout, text)
	return ExitOK
}

func (a *app) cmdSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", 30*time.Minute, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) < 1 {
		err = errors.New("expected a target")
	}
	if err != nil {
		return a.usageError("send", err)
	}
	text := strings.Join(pos[1:], " ")
	if len(pos) == 1 || (len(pos) == 2 && pos[1] == "-") {
		b, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
		if err != nil {
			return a.fail("send", err)
		}
		text = string(b)
	}
	ctx, cancel := signalContext()
	defer cancel()
	w, err := bind.Load(ctx)
	if err != nil {
		return a.fail("send", err)
	}
	ag, seq, err := query.Send(ctx, a.log, pos[0], text, query.Sender(w))
	if err != nil {
		return a.fail("send", err)
	}
	if !*wait {
		if a.json {
			a.emit(map[string]any{"agent": ag, "seq": seq})
		} else {
			fmt.Fprintf(a.stdout, "sent to %s (%s) seq %d\n", shortID(ag.ID), ag.Pane.ID, seq)
		}
		return ExitOK
	}
	return a.awaitAndReport("send", ag, query.WaitOptions{
		States: []string{adapter.Idle, adapter.Blocked}, After: seq, StartGrace: query.PendingWindow,
	}, *timeout)
}

func (a *app) cmdWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	states := fs.String("for", "idle,blocked", "")
	after := fs.Int64("after", -1, "")
	timeout := fs.Duration("timeout", 30*time.Minute, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = errors.New("expected one target")
	}
	want := splitList(*states)
	for _, s := range want {
		if s != adapter.Idle && s != adapter.Blocked && s != adapter.Working {
			err = fmt.Errorf("unknown state %q", s)
		}
	}
	if err != nil {
		return a.usageError("wait", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	// Read the cursor before the state: a transition landing in between is
	// then replayed instead of missed.
	cursor, err := a.log.Seq()
	if err != nil {
		return a.fail("wait", err)
	}
	ag, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.fail("wait", err)
	}
	if ag.ID == "" {
		return a.fail("wait", &query.RefusedError{Reason: query.RefuseUnbound, Detail: fmt.Sprintf("%s in %s has no verified session", ag.Agent, ag.Pane.ID)})
	}
	if *after < 0 {
		for _, s := range want {
			if ag.State == s {
				a.reportWait(ag, eventlog.Event{Seq: cursor, State: ag.State, Kind: ag.Kind, Turn: ag.Turn}, true)
				return ExitOK
			}
		}
		*after = cursor
	}
	return a.awaitAndReport("wait", ag, query.WaitOptions{States: want, After: *after}, *timeout)
}

func (a *app) awaitAndReport(cmd string, ag query.Agent, o query.WaitOptions, timeout time.Duration) int {
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()
	ev, err := query.Wait(ctx, a.log, ag, o)
	switch {
	case errors.Is(err, query.ErrNoTurn):
		// Delivered, but the agent started no turn (a slash command, say).
		a.reportWait(ag, eventlog.Event{State: adapter.Idle}, false)
		return ExitOK
	case errors.Is(err, context.DeadlineExceeded):
		err = fmt.Errorf("%w after %s waiting for %s", errTimeout, timeout, strings.Join(o.States, "|"))
	}
	if err != nil {
		return a.fail(cmd, err)
	}
	a.reportWait(ag, ev, true)
	return ExitOK
}

func (a *app) reportWait(ag query.Agent, ev eventlog.Event, started bool) {
	if a.json {
		a.emit(map[string]any{"id": ag.ID, "state": ev.State, "kind": ev.Kind, "turn": ev.Turn, "seq": ev.Seq, "started": started})
		return
	}
	state := ev.State
	if ev.Kind != "" {
		state += " (" + ev.Kind + ")"
	}
	if !started {
		state += " (no turn started)"
	}
	fmt.Fprintf(a.stdout, "%s %s\n", shortID(ag.ID), state)
}
