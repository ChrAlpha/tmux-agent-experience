package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// cmdStart starts an agent in a window beside the pane tmag runs in, waits
// until the pane shows its session, and gives it its first prompt as send
// does. The first argument names an agent, or a template the person set as
// @tmag-start-NAME.
func (a *app) cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	dir := fs.String("dir", "", "")
	branch := fs.String("worktree", "", "")
	wait := fs.Bool("wait", false, "")
	reply := fs.Bool("reply", false, "")
	timeout := fs.Duration("timeout", 2*time.Minute, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) < 1 {
		err = errors.New("expected an agent or a template")
	}
	if err == nil && *wait && *reply {
		err = errors.New("--reply has the peer prompt you later, which it cannot while --wait keeps your turn going; use one")
	}
	if err == nil && len(pos) == 1 && (*wait || *reply) {
		err = errors.New("--wait and --reply ask about a prompt: give one, or - for stdin")
	}
	if err != nil {
		return a.usageError("start", err)
	}
	o := query.StartOptions{Dir: *dir, Worktree: *branch, Ask: query.AskNoAnswer}
	if slices.Contains(proc.Agents, pos[0]) {
		o.Agent = pos[0]
	} else {
		o.Template = pos[0]
	}
	switch {
	case *reply:
		o.Ask = query.AskReply
	case *wait:
		o.Ask = query.AskFinal
	}
	if len(pos) > 1 {
		if o.Text, err = a.promptText(pos); err != nil {
			return a.failOn("start", pos[0], err)
		}
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()
	st, err := query.Start(ctx, a.log, o)
	var notShown *query.NotShownError
	switch {
	case query.IsUsage(err):
		return a.usageError("start", err)
	case errors.As(err, &notShown):
		return a.failOn("start", notShown.Pane.ID, notShownStart{err})
	case err != nil:
		return a.failOn("start", pos[0], err)
	}
	a.reportStart(st)
	if o.Ask != query.AskFinal {
		return ExitOK
	}
	ws := query.WaitOptions{States: []string{adapter.Idle, adapter.Blocked}}
	sent := st.Seq
	if d := st.Delivery; d != nil {
		prompted := d.Session.Turn
		ws.Prompted, ws.Turn, sent = &prompted, d.Turn, d.Seq
	}
	return a.awaitAndReportContext(ctx, "send", st.Session.ID, st.Session, sent, ws, *timeout)
}

// notShownStart marks a start whose agent showed no session in time.
type notShownStart struct{ err error }

func (e notShownStart) Error() string { return e.err.Error() }
func (e notShownStart) Unwrap() error { return e.err }

// reportStart prints what a start opened, and the delivery of its prompt.
func (a *app) reportStart(st query.Started) {
	where := paneName(tmux.DefaultServer().Socket, st.Pane.Server, st.Pane.ID)
	if a.json {
		doc := map[string]any{"pane": st.Pane, "session": st.Session, "dir": st.Dir, "seq": st.Seq}
		if d := st.Delivery; d != nil {
			doc["delivery"] = map[string]any{"via": d.Via, "seq": d.Seq, "held": d.Held}
		}
		a.emit(doc)
	} else {
		fmt.Fprintf(a.stdout, "started %s %s in %s (%s) seq %d\n", st.Session.Agent, shortID(st.Session.ID), where, st.Dir, st.Seq)
		if d := st.Delivery; d != nil {
			fmt.Fprintf(a.stdout, "sent to %s (%s) seq %d\n", shortID(d.Session.ID), d.Via, d.Seq)
		}
	}
	if d := st.Delivery; d != nil {
		a.reportHeld(*d)
	}
}
