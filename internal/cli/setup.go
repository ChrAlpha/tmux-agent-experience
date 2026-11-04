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

// setupPlan lists the commands that bring tmag's plugins in a host to the
// binary's version: tmag always, tmag-peers when peers is set, removed
// otherwise. Every command is idempotent: re-adding a marketplace or a
// plugin succeeds and installs the current version.
func setupPlan(h host, peers bool) [][]string {
	if !h.Present || !h.Listed {
		return nil
	}
	want := map[string]bool{pluginName: true, peersName: peers}
	var plan [][]string
	add := func(argv ...string) { plan = append(plan, argv) }

	// The marketplace is added with tmag, and refreshed before anything else
	// it provides is installed or updated.
	refresh := false
	for _, name := range []string{pluginName, peersName} {
		p := *h.of(name)
		refresh = refresh || want[name] && (!p.Installed || p.outdated())
	}
	switch {
	case !h.Tmag.Installed && h.CLI == "claude":
		add("claude", "plugin", "marketplace", "add", repo, "--sparse", ".claude-plugin", "plugins/claude")
	case !h.Tmag.Installed:
		add("codex", "plugin", "marketplace", "add", repo, "--sparse", ".agents", "--sparse", "plugins/codex")
	case refresh && h.CLI == "claude":
		add("claude", "plugin", "marketplace", "update", marketplace)
	case refresh:
		add("codex", "plugin", "marketplace", "upgrade", marketplace)
	}

	for _, name := range []string{pluginName, peersName} {
		p, id := *h.of(name), pluginID(name)
		switch {
		case !want[name] && p.Installed && h.CLI == "claude":
			add("claude", "plugin", "uninstall", id)
		case !want[name] && p.Installed:
			add("codex", "plugin", "remove", id)
		case !want[name]:
		case h.CLI == "claude" && !p.Installed:
			add("claude", "plugin", "install", id)
		case h.CLI == "claude":
			if p.outdated() {
				add("claude", "plugin", "update", id)
			}
			if !p.Enabled {
				add("claude", "plugin", "enable", id)
			}
		case !p.Installed || !p.Enabled || p.outdated():
			add("codex", "plugin", "add", id)
		}
	}
	return plan
}

// setupState says what tmag's plugins in a host are when nothing is left to do.
func setupState(h host, peers bool) string {
	if !peers {
		return fmt.Sprintf("tmag plugin %s is installed and enabled; tmag-peers is off (%s)", h.Tmag.Version, peersOption)
	}
	return fmt.Sprintf("tmag and tmag-peers plugins %s are installed and enabled", h.Tmag.Version)
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

	peers := peersWanted(context.Background())
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
		switch plan := setupPlan(h, peers); {
		case !h.Present:
			fmt.Fprintf(a.stdout, "%s: not installed, skipped\n", h.Name)
		case !h.Listed:
			fmt.Fprintf(a.stdout, "%s: cannot list its plugins; is %s up to date?\n", h.Name, h.CLI)
		case len(plan) == 0:
			fmt.Fprintf(a.stdout, "%s: %s\n", h.Name, setupState(h, peers))
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
