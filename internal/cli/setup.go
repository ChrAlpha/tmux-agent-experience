package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// setupPlan lists the commands that install or update the tmag plugin in a
// host. Every command is idempotent: re-adding a marketplace or a plugin
// succeeds and installs the current version.
func setupPlan(h host) [][]string {
	if !h.Present || !h.Listed {
		return nil
	}
	current := h.Installed && h.Enabled && (h.PluginVer == "" || h.PluginVer == version.Version)
	if current {
		return nil
	}
	switch h.CLI {
	case "claude":
		switch {
		case !h.Installed:
			return [][]string{
				{"claude", "plugin", "marketplace", "add", repo, "--sparse", ".claude-plugin", "plugins/claude"},
				{"claude", "plugin", "install", pluginID},
			}
		case !h.Enabled:
			return [][]string{{"claude", "plugin", "enable", pluginID}}
		}
		return [][]string{
			{"claude", "plugin", "marketplace", "update", marketplace},
			{"claude", "plugin", "update", pluginID},
		}
	case "codex":
		if !h.Installed {
			return [][]string{
				{"codex", "plugin", "marketplace", "add", repo, "--sparse", ".agents", "--sparse", "plugins/codex"},
				{"codex", "plugin", "add", pluginID},
			}
		}
		return [][]string{
			{"codex", "plugin", "marketplace", "upgrade", marketplace},
			{"codex", "plugin", "add", pluginID},
		}
	}
	return nil
}

func (a *app) cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	dry := fs.Bool("dry-run", false, "")
	only := fs.String("agent", "", "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err == nil && *only != "" && *only != "claude" && *only != "codex" {
		err = fmt.Errorf("unknown agent %q", *only)
	}
	if err != nil {
		return a.usageError("setup", err)
	}

	var hosts []host
	for _, probe := range []func() host{claudeHost, codexHost} {
		if h := probe(); *only == "" || h.CLI == *only {
			hosts = append(hosts, h)
		}
	}
	type step struct {
		host host
		cmds [][]string
	}
	var steps []step
	fmt.Fprintf(a.stdout, "tmag %s\n\n", version.Version)
	for _, h := range hosts {
		switch plan := setupPlan(h); {
		case !h.Present:
			fmt.Fprintf(a.stdout, "%s: not installed, skipped\n", h.Name)
		case !h.Listed:
			fmt.Fprintf(a.stdout, "%s: cannot list its plugins; is %s up to date?\n", h.Name, h.CLI)
		case len(plan) == 0:
			fmt.Fprintf(a.stdout, "%s: tmag plugin %s is installed and enabled\n", h.Name, h.PluginVer)
		default:
			fmt.Fprintf(a.stdout, "%s: will run\n", h.Name)
			for _, c := range plan {
				fmt.Fprintf(a.stdout, "    %s\n", strings.Join(c, " "))
			}
			steps = append(steps, step{h, plan})
		}
	}
	if len(steps) == 0 || *dry {
		a.setupNotes(hosts)
		return ExitOK
	}
	if !*yes {
		if !interactive() {
			fmt.Fprintln(a.stdout, "\nRun tmag setup --yes to apply.")
			return ExitOK
		}
		fmt.Fprint(a.stdout, "\nProceed? [Y/n] ")
		answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
		if reply := strings.ToLower(strings.TrimSpace(answer)); reply != "" && reply != "y" && reply != "yes" {
			fmt.Fprintln(a.stdout, "Nothing changed.")
			return ExitOK
		}
	}
	for _, s := range steps {
		for _, c := range s.cmds {
			fmt.Fprintf(a.stdout, "\n$ %s\n", strings.Join(c, " "))
			if err := a.runStep(c); err != nil {
				return a.fail("setup", fmt.Errorf("%s: %w", s.host.Name, err))
			}
		}
	}
	fmt.Fprintln(a.stdout)
	a.setupNotes(hosts)
	return ExitOK
}

func (a *app) runStep(argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = a.stdout, a.stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("%s exited with %d", argv[0], exit.ExitCode())
		}
		return err
	}
	return nil
}

// setupNotes tells the user what is left after the plugins are in place.
func (a *app) setupNotes(hosts []host) {
	for _, h := range hosts {
		if !h.Present {
			continue
		}
		switch h.CLI {
		case "claude":
			fmt.Fprintln(a.stdout, "Claude Code: start a new session in a tmux pane to load the plugin.")
		case "codex":
			fmt.Fprintln(a.stdout, "Codex: start a new session in a tmux pane and approve tmag's hooks with /hooks.")
			if pid, ok := codexDaemon(); ok {
				if problem := daemonBinaryProblem(pid); problem != "" {
					fmt.Fprintln(a.stdout, "Codex: "+problem)
				}
			}
		}
	}
}

func interactive() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
