package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// cmdNotify sends a test alert through @tmag-notify and reports what each
// command did. A hook starts it as `notify --alert`, with the alert and the
// command in its environment; it then runs the command and records a failure
// in hook.log, and always exits 0. Whatever ends a session starts it as
// `notify --ended SEQ`, which settles what that end leaves.
func (a *app) cmdNotify(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	fromHook := fs.Bool("alert", false, "")
	ended := fs.Int64("ended", 0, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err != nil {
		return a.usageError("notify", err)
	}
	if *ended > 0 {
		if err := a.settleEnd(*ended); err != nil {
			hook.RecordError(a.ctx, a.log.Dir, "notify", err)
		}
		return ExitOK
	}
	if *fromHook {
		if err := notify.RunAlert(a.log.Dir, os.Environ()); err != nil {
			hook.RecordError(a.ctx, a.log.Dir, "notify", err)
		}
		return ExitOK
	}

	ctx, cancel := a.signalContext()
	defer cancel()
	commands := notifyCommands(ctx)
	if len(commands) == 0 {
		return a.fail("notify", errors.New(notify.Option+" is not set on any tmux server: set it to a command that sends a notification"))
	}
	host, _ := os.Hostname()
	alert := notify.Alert{Kind: notify.Test, Host: host}
	type ran struct {
		Command string `json:"command"`
		Output  string `json:"output"`
		Error   string `json:"error,omitempty"`
	}
	var results []ran
	failed := false
	for _, command := range commands {
		run, cancel := context.WithTimeout(ctx, notify.Timeout)
		out, err := notify.Run(run, a.log.Dir, command, append(os.Environ(), alert.Env()...))
		cancel()
		r := ran{Command: command, Output: string(bytes.TrimSpace(out))}
		if err != nil {
			r.Error, failed = err.Error(), true
		}
		results = append(results, r)
	}
	if a.json {
		a.emit(map[string]any{"alert": alert.Kind, "ran": results})
	} else {
		for _, r := range results {
			if r.Output != "" {
				fmt.Fprintln(a.stdout, r.Output)
			}
			if r.Error != "" {
				fmt.Fprintf(a.stderr, "tmag notify: %s\n", r.Error)
			} else {
				fmt.Fprintf(a.stdout, "sent a test alert through %s\n", notify.Option)
			}
		}
	}
	if failed {
		return ExitError
	}
	return ExitOK
}

// notifyCommands are the distinct @tmag-notify commands the tmux servers
// set. Clients build any notification links from the exported ids.
func notifyCommands(ctx context.Context) (commands []string) {
	servers, _ := tmux.Servers(ctx)
	for _, srv := range servers {
		command, _ := srv.Option(ctx, notify.Option)
		if command == "" || slices.Contains(commands, command) {
			continue
		}
		commands = append(commands, command)
	}
	return commands
}

// checkNotify reports configured delivery; tmag notify tests the command.
func (a *app) checkNotify(ctx context.Context) check {
	servers, err := doctorServers(ctx)
	if err != nil {
		return check{"notify", "warn", "server discovery incomplete"}
	}
	configured := false
	for _, s := range servers {
		v, err := s.Option(ctx, notify.Option)
		if err != nil {
			return check{"notify", "warn", "notification option unavailable"}
		}
		configured = configured || v != ""
	}
	if !configured {
		return check{"notify", "skip", notify.Option + " is not set: alerts ring tmux's bell only"}
	}
	return check{"notify", "ok", fmt.Sprintf("%s configured; tmag notify tests delivery", notify.Option)}
}

// settleEnd settles what the end tmag logged at seq leaves: what other
// sessions owed it is closed, whenever this runs, and within
// session.EndWindow of the end each agent the session owed a reply is told,
// once whoever runs this, that none will come. A notice whose delivery is
// unknown is not sent again. Every busy asker is tried again each round
// until the window closes, so one that stays busy holds no other up.
func (a *app) settleEnd(seq int64) error {
	ended, askers, until, err := query.Ending(a.ctx, a.log, seq)
	if err != nil {
		return err
	}
	forget, cancelForget := context.WithTimeout(a.ctx, session.EndWindow)
	defer cancelForget()
	errs := []error{query.Forget(forget, a.log, ended)}
	if !time.Now().Before(until) {
		return errors.Join(errs...) // the window to tell its askers closed
	}
	ctx, cancel := context.WithDeadline(a.ctx, until)
	defer cancel()
	failed := func(asker eventlog.Actor, err error) {
		errs = append(errs, fmt.Errorf("telling %s that %s ended: %w", asker.Session, ended.Session, err))
	}
	for pending := askers; len(pending) > 0; {
		var busy []eventlog.Actor
		for _, asker := range pending {
			_, err := query.Tell(ctx, a.log, asker, ended, seq)
			var refused *query.RefusedError
			switch {
			case err == nil, errors.Is(err, query.ErrTold):
			case errors.As(err, &refused) && refused.Reason == query.RefuseBusy:
				busy = append(busy, asker)
			default:
				failed(asker, err)
			}
		}
		if len(busy) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			for _, asker := range busy {
				failed(asker, ctx.Err())
			}
			return errors.Join(errs...)
		case <-time.After(replyRetry):
		}
		pending = busy
	}
	return errors.Join(errs...)
}
