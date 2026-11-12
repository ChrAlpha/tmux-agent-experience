package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// cmdSeen marks a turn seen by the person: one the terminal shows, with
// --shown, or the turn a target names. Agents never mark a turn seen.
func (a *app) cmdSeen(args []string) int {
	fs := flag.NewFlagSet("seen", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	turn := fs.String("turn", "", "")
	shown := fs.Bool("shown", false, "")
	server := fs.String("server", "", "")
	pos, err := parse(fs, args)
	switch {
	case err != nil:
	case *shown && len(pos) > 0:
		err = errors.New("--shown marks the turns the terminal shows; it takes no target")
	case *shown && *turn != "":
		err = errors.New("--shown marks the turns the terminal shows; drop --turn")
	case *shown && flagSet(fs, "json"):
		err = errors.New("--shown prints nothing; drop --json")
	case !*shown && *server != "":
		err = errors.New("--server goes with --shown")
	case !*shown && len(pos) != 1:
		err = errors.New("expected one target, or --shown")
	case len(*turn) > session.MaxTurn:
		err = fmt.Errorf("--turn is longer than %d bytes", session.MaxTurn)
	}
	if err != nil {
		return a.usageError("seen", err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	if *shown {
		ctx, cancelTimeout := context.WithTimeout(ctx, 2*time.Second)
		defer cancelTimeout()
		v, w, err := a.reconciled(ctx, *server)
		if err != nil {
			return a.fail("seen", err)
		}
		if _, err := query.MarkShown(ctx, a.log, v, w, *server); err != nil {
			return a.fail("seen", err)
		}
		return ExitOK
	}
	d, err := query.Seen(ctx, a.log, pos[0], *turn)
	if err != nil {
		return a.failOn("seen", pos[0], err)
	}
	a.reportSeen(d)
	return ExitOK
}

// reportSeen prints the session and the turn it now reports seen, and the
// event when one was logged.
func (a *app) reportSeen(d query.Delivery) {
	if a.json {
		a.emit(map[string]any{"session": d.Session, "pane": d.Pane, "via": d.Via, "seq": d.Seq})
		return
	}
	fmt.Fprintf(a.stdout, "seen %s (turn %s)", shortID(d.Session.ID), d.Session.Seen)
	if d.Seq != 0 {
		fmt.Fprintf(a.stdout, " seq %d", d.Seq)
	}
	fmt.Fprintln(a.stdout)
}
