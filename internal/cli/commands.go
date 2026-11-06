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
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
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
		a.emit(map[string]any{"seq": snap.Seq, "caller": snap.Caller, "sessions": snap.Sessions, "panes": snap.Panes})
		return ExitOK
	}
	a.printView(snap.View, snap.Caller)
	if hint := codexHint(snap.View); hint != "" {
		fmt.Fprintln(a.stderr, "\n"+hint)
	}
	return ExitOK
}

// printView lists every agent pane, then every session no pane shows,
// marking the caller's own session.
func (a *app) printView(v query.View, caller query.Caller) {
	if len(v.Panes) == 0 && len(v.Sessions) == 0 {
		fmt.Fprintln(a.stdout, "no agents")
		return
	}
	servers := map[string]bool{}
	for _, p := range v.Panes {
		servers[p.Server] = true
	}
	home, _ := os.UserHomeDir()
	short := func(path string) string {
		if home != "" && strings.HasPrefix(path, home) {
			return "~" + strings.TrimPrefix(path, home)
		}
		return path
	}
	own := func(s query.Session) string {
		if caller.Is(s) {
			return shortID(s.ID) + " (you)"
		}
		return shortID(s.ID)
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tPANE\tAGENT\tSTATE\tFOR\tID\tCWD")
	for _, p := range v.Panes {
		target := p.Target
		if len(servers) > 1 {
			target = filepath.Base(p.Server) + "/" + target
		}
		state, since, id := adapter.Unknown, (*time.Time)(nil), "-"
		if s, ok := v.Session(p.Agent, p.Session); ok {
			state, since, id = describe(s.State, s.Kind), s.Since, own(s)
			if p.View == attach.ViewSubagent {
				state += " [subagent]"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", target, p.ID, p.Agent, state, age(since), id, short(p.Cwd))
	}
	for _, s := range v.Sessions {
		if len(s.Panes) == 0 {
			fmt.Fprintf(tw, "-\t-\t%s\t%s\t%s\t%s\t%s\n", s.Agent, describe(s.State, s.Kind), age(s.Since), own(s), short(s.Cwd))
		}
	}
	tw.Flush()
}

// codexHint explains how to let Codex panes show their sessions, when some
// Codex pane shows none while a Codex session runs in the shared daemon.
func codexHint(v query.View) string {
	unshown := false
	for _, p := range v.Panes {
		unshown = unshown || (p.Agent == proc.Codex && p.Session == "")
	}
	for _, s := range v.Sessions {
		if unshown && s.Agent == proc.Codex && s.Runtime == string(proc.Service) && len(s.Panes) == 0 {
			return codexTitleHint
		}
	}
	return ""
}

// codexTitleHint is the one setting Codex needs for tmag to see which pane
// shows which thread when threads run in its shared daemon.
const codexTitleHint = "Codex runs these threads in its shared daemon, where only the terminal title can show which pane displays which. " +
	"In Codex, run /title and turn on thread-id once; or launch codex --no-daemon."

func describe(state, kind string) string {
	if kind != "" {
		return state + " (" + kind + ")"
	}
	return state
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

func (a *app) resolve(ctx context.Context, target string) (query.Target, error) {
	v, _, err := query.Load(ctx, a.store())
	if err != nil {
		return query.Target{}, err
	}
	return query.Resolve(v, target)
}

func (a *app) store() session.Store { return session.Store{Dir: a.log.Dir} }

// needSession returns the session a target names, or a refusal for a pane
// that shows none.
func needSession(t query.Target) (query.Session, error) {
	if t.Session != nil {
		return *t.Session, nil
	}
	return query.Session{}, &query.RefusedError{Reason: query.RefuseUnbound, Detail: fmt.Sprintf("%s in %s shows no session tmag can prove", t.Pane.Agent, t.Pane.ID)}
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
	t, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.fail("inspect", err)
	}
	events := []eventlog.Event{}
	if t.Session != nil {
		if events, err = query.SessionEvents(ctx, a.log, t.Session.ID, *n); err != nil {
			return a.fail("inspect", err)
		}
		if events == nil {
			events = []eventlog.Event{}
		}
	}
	if a.json {
		a.emit(map[string]any{"session": t.Session, "pane": t.Pane, "events": events})
		return ExitOK
	}
	v := query.View{}
	if t.Pane != nil {
		v.Panes = []query.Pane{*t.Pane}
	}
	if t.Session != nil {
		v.Sessions = []query.Session{*t.Session}
		if t.Pane == nil {
			v.Sessions[0].Panes = nil // list it on its own row
		}
	}
	a.printView(v, query.Caller{})
	if s := t.Session; s != nil {
		fmt.Fprintf(a.stdout, "\nid: %s\nruntime: %s\n", s.ID, s.Runtime)
		for _, p := range s.Panes {
			fmt.Fprintf(a.stdout, "shown in: %s (%s, %s)\n", p.ID, p.Evidence, p.View)
		}
	}
	for _, e := range events {
		fmt.Fprintf(a.stdout, "%6d  %s  %-18s %s\n", e.Seq, e.Time.Local().Format("15:04:05"), e.Event, describe(e.State, e.Kind))
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
	// A reader that goes away without a word, as a client over ssh does when
	// its connection drops, would otherwise go unnoticed until the next
	// write, which may not come for hours.
	if f, ok := a.stdout.(*os.File); ok {
		if gone := hangup(f, ctx.Done()); gone != nil {
			go func() {
				select {
				case <-gone:
					stop(errReaderGone)
				case <-ctx.Done():
				}
			}()
		}
	}

	out := bufio.NewWriter(a.stdout)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	// mu orders output: events, and pane lists written between them.
	var mu sync.Mutex
	write := func(doc map[string]any) error {
		doc["schema"] = version.Schema
		if err := enc.Encode(doc); err != nil {
			return err
		}
		return out.Flush()
	}
	var shown string // the pane list last sent
	panes := func(list []query.Pane) error {
		if b, _ := json.Marshal(list); string(b) != shown {
			shown = string(b)
			return write(map[string]any{"type": "panes", "panes": list})
		}
		return nil
	}

	snapshot := func() (int64, error) {
		snap, err := query.TakeSnapshot(ctx, a.log)
		if err != nil {
			return 0, err
		}
		b, _ := json.Marshal(snap.Panes)
		shown = string(b)
		return snap.Seq, write(map[string]any{"type": "snapshot", "seq": snap.Seq, "sessions": snap.Sessions, "panes": snap.Panes})
	}
	// A resumed watch replays what it missed and then sends the pane list;
	// when the log no longer holds all of it, it starts over with a snapshot.
	var last int64
	resumed := false
	if *since >= 0 {
		events, complete, err := a.log.Since(ctx, *since)
		if err != nil {
			return a.fail("watch", err)
		}
		if resumed = complete; resumed {
			last = *since
			for _, e := range events {
				if err := write(map[string]any{"type": "event", "event": e}); err != nil {
					return a.watchEnd(err)
				}
				last = e.Seq
			}
			v, _, err := query.Load(ctx, a.store())
			if err == nil {
				err = panes(v.Panes)
			}
			if err != nil {
				return a.watchEnd(err)
			}
		}
	}
	if !resumed {
		if last, err = snapshot(); err != nil {
			return a.fail("watch", err)
		}
	}

	// Sessions change only through events. What changes without one is
	// found by reconciling, which records it as an event (a pane stopped or
	// started showing a session, a session's process exited), and by the
	// pane list itself (an agent started in a pane, a pane moved), sent
	// whenever it differs from the last one sent.
	env := attach.Env{Log: a.log, Store: a.store()}
	go func() {
		tick := time.NewTicker(watchRescan)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			w, err := attach.Load(ctx)
			if err != nil {
				continue
			}
			attach.Reconcile(ctx, env, w)
			v, _, err := query.Load(ctx, a.store())
			if err != nil {
				continue
			}
			mu.Lock()
			err = panes(v.Panes)
			mu.Unlock()
			if err != nil {
				stop(err)
				return
			}
		}
	}()

	err = a.log.Follow(ctx, last, 200*time.Millisecond, func(e eventlog.Event) error {
		mu.Lock()
		defer mu.Unlock()
		return write(map[string]any{"type": "event", "event": e})
	})
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	return a.watchEnd(err)
}

func (a *app) watchEnd(err error) int {
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errReaderGone) && !isClosedPipe(err) {
		return a.fail("watch", err)
	}
	return ExitOK
}

// watchRescan is how often watch looks for changes that come without an event.
const watchRescan = 2 * time.Second

func isClosedPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}

func (a *app) cmdRead(args []string) int {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	lines := fs.Int("lines", 100, "")
	last := fs.Bool("last", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = errors.New("expected one target")
	}
	if err == nil && *lines <= 0 {
		err = errors.New("--lines must be positive")
	}
	if err == nil && *last && flagSet(fs, "lines") {
		err = errors.New("--last reads the agent's reply, not lines of its pane; drop --lines")
	}
	if err != nil {
		return a.usageError("read", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	if *last {
		s, r, err := query.LastReply(ctx, a.store(), pos[0])
		if err != nil {
			return a.fail("read", err)
		}
		if a.json {
			a.emit(map[string]any{"session": s, "turn": r.Turn, "text": r.Text})
			return ExitOK
		}
		fmt.Fprintln(a.stdout, strings.TrimRight(r.Text, "\n"))
		return ExitOK
	}
	t, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.fail("read", err)
	}
	if t.Pane == nil {
		return a.fail("read", fmt.Errorf("%w: %s is shown by %d panes; name one", query.ErrAmbiguous, t.Session.ID, len(t.Session.Panes)))
	}
	text, err := tmux.Server{Socket: t.Pane.Server}.Capture(ctx, t.Pane.ID, *lines)
	if err != nil {
		return a.fail("read", err)
	}
	if a.json {
		a.emit(map[string]any{"pane": t.Pane, "lines": *lines, "text": text})
		return ExitOK
	}
	fmt.Fprint(a.stdout, text)
	return ExitOK
}

// flagSet reports whether the command line set the named flag.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// promptText is the prompt a command was given: its words after the
// target, or stdin when there are none or only "-".
func (a *app) promptText(pos []string) (string, error) {
	if len(pos) == 1 || (len(pos) == 2 && pos[1] == "-") {
		b, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
		return string(b), err
	}
	return strings.Join(pos[1:], " "), nil
}

func (a *app) cmdSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	wait := fs.Bool("wait", false, "")
	reply := fs.Bool("reply", false, "")
	timeout := fs.Duration("timeout", 30*time.Minute, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) < 1 {
		err = errors.New("expected a target")
	}
	if err == nil && *wait && *reply {
		err = errors.New("--reply has the peer prompt you later, which it cannot while --wait keeps your turn going; use one")
	}
	if err != nil {
		return a.usageError("send", err)
	}
	text, err := a.promptText(pos)
	if err != nil {
		return a.fail("send", err)
	}
	ask := query.AskFinal
	if *reply {
		ask = query.AskReply
	}
	ctx, cancel := signalContext()
	defer cancel()
	d, err := query.Send(ctx, a.log, pos[0], text, ask)
	if err != nil {
		return a.fail("send", err)
	}
	if !*wait {
		a.reportDelivery("sent to", d)
		return ExitOK
	}
	return a.awaitAndReport("send", d.Session, query.WaitOptions{
		States: []string{adapter.Idle, adapter.Blocked}, After: d.Seq, StartGrace: query.PendingWindow,
	}, *timeout)
}

// cmdReply answers a prompt that asked for a reply: the peer gets it as a
// prompt whose first line says it is one. A peer still busy is waited for,
// until --timeout, and prompted once it is idle.
func (a *app) cmdReply(args []string) int {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	timeout := fs.Duration("timeout", time.Minute, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) < 1 {
		err = errors.New("expected a target")
	}
	if err != nil {
		return a.usageError("reply", err)
	}
	text, err := a.promptText(pos)
	if err != nil {
		return a.fail("reply", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()
	for {
		// Read the cursor before trying: the peer turning idle in between
		// is then seen rather than waited for.
		cursor, err := a.log.Seq()
		if err != nil {
			return a.fail("reply", err)
		}
		d, err := query.Send(ctx, a.log, pos[0], text, query.Answering)
		if err == nil {
			a.reportDelivery("replied to", d)
			return ExitOK
		}
		var refused *query.RefusedError
		if !errors.As(err, &refused) || refused.Reason != query.RefuseBusy {
			return a.fail("reply", a.deadline(ctx, *timeout, err))
		}
		t, err := a.resolve(ctx, pos[0])
		if err == nil {
			_, err = needSession(t)
		}
		if err != nil {
			return a.fail("reply", a.deadline(ctx, *timeout, err))
		}
		// Wait for the peer's next idle, or a pending window at most: a
		// prompt tmag just delivered may start no turn.
		wait, stop := context.WithTimeout(ctx, query.PendingWindow)
		_, err = query.Wait(wait, a.log, *t.Session, query.WaitOptions{States: []string{adapter.Idle}, After: cursor})
		stop()
		if ctx.Err() != nil {
			return a.fail("reply", a.deadline(ctx, *timeout, refused))
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return a.fail("reply", err)
		}
	}
}

// deadline reports err as a timeout when ctx ran out of time. It keeps
// only err's message: a refusal it wraps would report the timeout as one.
func (a *app) deadline(ctx context.Context, timeout time.Duration, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s, nothing delivered: %s", errTimeout, timeout, err.Error())
	}
	return err
}

func (a *app) cmdInterrupt(args []string) int {
	fs := flag.NewFlagSet("interrupt", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = errors.New("expected one target")
	}
	if err != nil {
		return a.usageError("interrupt", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	d, err := query.Interrupt(ctx, a.log, pos[0])
	if err != nil {
		return a.fail("interrupt", err)
	}
	a.reportDelivery("interrupted", d)
	return ExitOK
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
	t, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.fail("wait", err)
	}
	s, err := needSession(t)
	if err != nil {
		return a.fail("wait", err)
	}
	if *after < 0 {
		for _, st := range want {
			if s.State == st {
				a.reportWait(s, eventlog.Event{Seq: cursor, State: s.State, Kind: s.Kind, Turn: s.Turn}, true)
				return ExitOK
			}
		}
		*after = cursor
	}
	return a.awaitAndReport("wait", s, query.WaitOptions{States: want, After: *after}, *timeout)
}

func (a *app) awaitAndReport(cmd string, s query.Session, o query.WaitOptions, timeout time.Duration) int {
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()
	ev, err := query.Wait(ctx, a.log, s, o)
	switch {
	case errors.Is(err, query.ErrNoTurn):
		// Delivered, but the agent started no turn (a slash command, say).
		a.reportWait(s, eventlog.Event{State: adapter.Idle}, false)
		return ExitOK
	case errors.Is(err, context.DeadlineExceeded):
		err = fmt.Errorf("%w after %s waiting for %s", errTimeout, timeout, strings.Join(o.States, "|"))
	}
	if err != nil {
		return a.fail(cmd, err)
	}
	a.reportWait(s, ev, true)
	return ExitOK
}

func (a *app) reportWait(s query.Session, ev eventlog.Event, started bool) {
	if a.json {
		a.emit(map[string]any{"id": s.ID, "state": ev.State, "kind": ev.Kind, "turn": ev.Turn, "seq": ev.Seq, "started": started})
		return
	}
	state := describe(ev.State, ev.Kind)
	if !started {
		state += " (no turn started)"
	}
	fmt.Fprintf(a.stdout, "%s %s\n", shortID(s.ID), state)
}

// reportDelivery prints where a verb went.
func (a *app) reportDelivery(what string, d query.Delivery) {
	if a.json {
		a.emit(map[string]any{"session": d.Session, "pane": d.Pane, "via": d.Via, "seq": d.Seq})
		return
	}
	where := d.Via
	if d.Pane != nil {
		where = d.Pane.ID
	}
	fmt.Fprintf(a.stdout, "%s %s (%s) seq %d\n", what, shortID(d.Session.ID), where, d.Seq)
}
