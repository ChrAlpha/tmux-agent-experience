package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// cmdNotify sends a test alert through @tmag-notify and reports what each
// command did. A hook starts it as `notify --alert`, with the alert and the
// command in its environment; it then runs the command and records a failure
// in hook.log, and always exits 0.
func (a *app) cmdNotify(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	fromHook := fs.Bool("alert", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err != nil {
		return a.usageError("notify", err)
	}
	if *fromHook {
		if err := notify.RunAlert(a.log.Dir, os.Environ()); err != nil {
			hook.RecordError(a.log.Dir, "notify", err)
		}
		return ExitOK
	}

	ctx := context.Background()
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
		out, err := notify.RunBounded(run, a.log.Dir, command, append(os.Environ(), alert.Env()...))
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
	for _, srv := range tmux.Servers() {
		command := srv.GlobalOption(ctx, notify.Option)
		if command == "" || slices.Contains(commands, command) {
			continue
		}
		commands = append(commands, command)
	}
	return commands
}

// checkNotify reports whether alerts reach the person away from the
// terminal, and the latest failure of the command.
func (a *app) checkNotify(ctx context.Context) check {
	servers, err := tmux.ServersLimitContext(ctx, policy.MaxServers)
	if err != nil {
		return check{"notify", "warn", "server discovery incomplete"}
	}
	var commands []string
	for _, srv := range servers {
		command, err := srv.GlobalOptionContext(ctx, notify.Option)
		if err != nil {
			if absent, probeErr := srv.Absent(ctx); probeErr == nil && absent {
				continue
			}
			return check{"notify", "warn", "notification option lookup unavailable"}
		}
		if command != "" {
			commands = append(commands, "configured")
		}
	}
	if len(commands) == 0 {
		return check{"notify", "skip", notify.Option + " is not set: alerts ring tmux's bell only"}
	}

	return check{"notify", "ok", fmt.Sprintf("%s is set; `tmag notify` sends a test alert", notify.Option)}
}
