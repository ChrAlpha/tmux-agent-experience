package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
)

// cmdHook always exits 0 and never writes stdout: a broken tmag must not
// disturb the agent. Failures go to <state dir>/hook.log.
func (a *app) cmdHook(args []string) (code int) {
	if len(args) != 1 || !proc.IsAgent(args[0]) {
		fmt.Fprintln(a.stderr, "usage: tmag hook <"+strings.Join(proc.Agents, "|")+">")
		return ExitOK
	}
	ctx, cancel := a.hookContext()
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			hook.RecordErrorContext(ctx, a.log.Dir, args[0], fmt.Errorf("panic: %v", r))
			code = ExitOK
		}
	}()
	err := hook.RunContext(ctx, hook.Input{Agent: args[0], Payload: a.stdin, Log: a.log, PID: os.Getpid()})
	if err != nil {
		hook.RecordErrorContext(ctx, a.log.Dir, args[0], err)
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
	ctx, cancel := a.signalContext()
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
	// Targets resolve on the caller's tmux server; a pane on another is
	// named server:%N.
	here := tmux.DefaultServer().Socket
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
		if p.Server != here {
			target = filepath.Base(p.Server) + "/" + target
		}
		pane := paneName(here, p.Server, p.ID)
		state, since, id := adapter.Unknown, (*time.Time)(nil), "-"
		if s, ok := v.Session(p.Agent, p.Session); ok {
			state, since, id = describe(s.State, s.Kind), s.Since, own(s)
			if p.View == attach.ViewSubagent {
				state += " [subagent]"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", target, pane, p.Agent, state, age(since), id, short(p.Cwd))
	}
	for _, s := range v.Sessions {
		if len(s.Panes) == 0 {
			fmt.Fprintf(tw, "-\t-\t%s\t%s\t%s\t%s\t%s\n", s.Agent, describe(s.State, s.Kind), age(s.Since), own(s), short(s.Cwd))
		}
	}
	tw.Flush()
}

// paneName names a pane as targets take it: %N on the caller's tmux server
// (here), server:%N on another.
func paneName(here, server, id string) string {
	if server == here {
		return id
	}
	return serverName(server) + ":" + id
}

// serverName names a tmux server as targets take it: its socket's name when
// it is in tmux's socket directory, else its path.
func serverName(socket string) string {
	if filepath.Dir(socket) == tmux.SocketDir() {
		return filepath.Base(socket)
	}
	return socket
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
	return query.Resolve(ctx, v, target)
}

func (a *app) store() session.Store { return session.Store{Dir: a.log.Dir} }

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
	ctx, cancel := a.signalContext()
	defer cancel()
	t, err := a.resolve(ctx, pos[0])
	if err != nil {
		return a.failOn("inspect", pos[0], err)
	}
	events := []eventlog.Event{}
	if t.Session != nil {
		if events, err = query.SessionEvents(ctx, a.log, t.Session.ID, *n); err != nil {
			return a.failOn("inspect", pos[0], err)
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
		if !s.LastHookAt.IsZero() {
			fmt.Fprintf(a.stdout, "last hook: %s (contact only)\n", s.LastHookAt.UTC().Format(time.RFC3339Nano))
		}
		here := tmux.DefaultServer().Socket
		for _, p := range s.Panes {
			fmt.Fprintf(a.stdout, "shown in: %s (%s, %s)\n", paneName(here, p.Server, p.ID), p.Evidence, p.View)
		}
		// Open requests, by the ids request and answer take.
		for _, r := range s.Requests {
			what := r.Kind
			if r.Tool != "" {
				what += ", " + r.Tool
			}
			if r.Agent != "" {
				what += ", subagent " + r.Agent
			}
			fmt.Fprintf(a.stdout, "request: %s (%s)\n", r.ID, what)
		}
	}
	for _, e := range events {
		fmt.Fprintf(a.stdout, "%6d  %s  %-18s %s\n", e.Seq, e.Time.Local().Format("15:04:05"), e.Event, describe(e.State, e.Kind))
	}
	return ExitOK
}

func (a *app) signalContext() (context.Context, context.CancelFunc) {
	if a.ctx != nil {
		return context.WithCancel(a.ctx)
	}
	return signalContext()
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
	ctx, cancel := a.signalContext()
	defer cancel()
	if *last {
		s, r, err := query.LastReply(ctx, a.store(), pos[0])
		if err != nil {
			return a.failOn("read", pos[0], err)
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
		return a.failOn("read", pos[0], err)
	}
	if t.Pane == nil {
		return a.failOn("read", pos[0], fmt.Errorf("%w: %s is shown by %d panes; name one", query.ErrAmbiguous, t.Session.ID, len(t.Session.Panes)))
	}
	text, err := tmux.Server{Socket: t.Pane.Server}.Capture(ctx, t.Pane.ID, *lines)
	if err != nil {
		return a.failOn("read", pos[0], err)
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
		return a.failOn("send", pos[0], err)
	}
	ask := query.AskNoAnswer
	switch {
	case *reply:
		ask = query.AskReply
	case *wait:
		ask = query.AskFinal
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	d, err := query.Send(ctx, a.log, pos[0], text, ask)
	if err != nil {
		return a.failOn("send", pos[0], err)
	}
	if !*wait {
		a.reportDelivery("sent to", d)
		return ExitOK
	}
	prompted := d.Session.Turn
	return a.awaitAndReport("send", pos[0], d.Session, d.Seq, query.WaitOptions{
		States: []string{adapter.Idle, adapter.Blocked}, Prompted: &prompted,
	}, *timeout)
}

// replyRetry is how often a reply tries a busy asker again.
const replyRetry = time.Second

// cmdReply answers a prompt that asked for a reply: the peer gets it through
// its own message channel, or through its pane while idle. A pane-only peer
// that is busy is tried again every replyRetry, until --timeout.
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
		return a.failOn("reply", pos[0], err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()
	for {
		d, err := query.Reply(ctx, a.log, pos[0], text)
		if err == nil {
			a.reportDelivery("replied to", d)
			return ExitOK
		}
		var refused *query.RefusedError
		if !errors.As(err, &refused) || refused.Reason != query.RefuseBusy {
			return a.failOn("reply", pos[0], a.deadline(ctx, *timeout, err))
		}
		select {
		case <-ctx.Done():
			return a.failOn("reply", pos[0], a.deadline(ctx, *timeout, refused))
		case <-time.After(replyRetry):
		}
	}
}

// deadline reports err as a timeout when ctx ran out of time. It keeps
// only err's message: a refusal it wraps would report the timeout as one.
// A refusal delivers nothing, so after one the reply may run again; any
// other error may have come after the reply went out.
func (a *app) deadline(ctx context.Context, timeout time.Duration, err error) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if errors.As(err, new(*query.RefusedError)) {
		return undelivered{fmt.Errorf("%w after %s, nothing delivered: %s", errTimeout, timeout, err.Error())}
	}
	return fmt.Errorf("%w after %s; whether the reply was delivered is unknown: %s", errTimeout, timeout, err.Error())
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
	ctx, cancel := a.signalContext()
	defer cancel()
	d, err := query.Interrupt(ctx, a.log, pos[0])
	if err != nil {
		return a.failOn("interrupt", pos[0], err)
	}
	a.reportDelivery("interrupted", d)
	return ExitOK
}

func (a *app) cmdWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	states := fs.String("for", "idle,blocked", "")
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
	ctx, cancel := a.signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()
	s, err := a.shown(ctx, pos[0], *timeout)
	if err != nil {
		return a.failOn("wait", pos[0], err)
	}
	return a.awaitAndReportContext(ctx, "wait", pos[0], s, 0, query.WaitOptions{States: want}, *timeout)
}

// shown is the session target names. A pane that shows none yet, its agent
// just started there or not yet running, is waited on until it shows one,
// within ctx.
func (a *app) shown(ctx context.Context, target string, timeout time.Duration) (query.Session, error) {
	v, _, err := query.Load(ctx, a.store())
	if err != nil {
		return query.Session{}, err
	}
	t, err := query.Resolve(ctx, v, target)
	var ref eventlog.PaneRef
	switch {
	case err == nil && t.Session != nil:
		return *t.Session, nil
	case err == nil:
		ref = eventlog.PaneRef{Server: t.Pane.Server, ID: t.Pane.ID}
	case errors.Is(err, query.ErrNotFound):
		// A pane whose agent does not run yet.
		var perr error
		if ref, perr = query.PaneOf(ctx, target); perr != nil {
			return query.Session{}, err
		}
	default:
		return query.Session{}, err
	}
	s, err := query.WaitShown(ctx, a.store(), ref)
	if errors.Is(err, context.DeadlineExceeded) {
		err = notShown{fmt.Errorf("%w after %s: %s shows no session tmag can prove", errTimeout, timeout, target)}
	}
	return s, err
}

func (a *app) awaitAndReport(cmd, target string, s query.Session, sent int64, o query.WaitOptions, timeout time.Duration) int {
	ctx, cancel := a.signalContext()
	defer cancel()
	return a.awaitAndReportContext(ctx, cmd, target, s, sent, o, timeout)
}

// awaitAndReportContext waits for s as o says and reports the state it
// reached; cmd and target are the command and the target as typed, and sent
// the event of the prompt send delivered, for a failure to report.
func (a *app) awaitAndReportContext(ctx context.Context, cmd, target string, s query.Session, sent int64, o query.WaitOptions, timeout time.Duration) int {
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()
	rec, err := query.Wait(ctx, a.store(), s, o)
	switch {
	case errors.Is(err, query.ErrNoTurn):
		// Delivered, but the agent started no turn (a slash command, say).
		a.reportWait(s, rec.State(), rec.Kind(), rec.Turn, false)
		return ExitOK
	case errors.Is(err, context.DeadlineExceeded):
		err = fmt.Errorf("%w after %s waiting for %s", errTimeout, timeout, strings.Join(o.States, "|"))
	}
	if err != nil {
		if cmd == "send" {
			err = delivered{fmt.Errorf("prompt delivered at seq %d; subsequent outcome unknown, do not repeat delivery: %w", sent, err)}
		}
		return a.failOn(cmd, target, err)
	}
	a.reportWait(s, rec.State(), rec.Kind(), rec.Turn, true)
	return ExitOK
}

func (a *app) reportWait(s query.Session, state, kind, turn string, started bool) {
	if a.json {
		a.emit(map[string]any{"id": s.ID, "state": state, "kind": kind, "turn": turn, "started": started})
		return
	}
	state = describe(state, kind)
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
		where = paneName(tmux.DefaultServer().Socket, d.Pane.Server, d.Pane.ID)
	}
	fmt.Fprintf(a.stdout, "%s %s (%s) seq %d\n", what, shortID(d.Session.ID), where, d.Seq)
}
