package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// removeState removes everything in a state directory but its lock files,
// which are never removed: a process that holds one keeps excluding the
// others.
func removeState(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if lockfile.Named(e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("uninstall", err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	if *purge {
		if err := (auth.Store{Dir: auth.DefaultDir()}).Revoke(ctx, "", true); err != nil {
			return a.fail("uninstall", err)
		}
		// Agents keep running, and their hooks with them. The state goes
		// first, but for its lock files, which keep a hook that runs
		// meanwhile excluded; then each pane is cleared under its lock, so
		// what a hook attached before is gone, and what it attaches after
		// is in a record written after the state went.
		if err := removeState(a.log.Dir); err != nil {
			return a.fail("uninstall", err)
		}
		cleared := 0
		servers, _ := tmux.Servers(ctx)
		for _, s := range servers {
			panes, err := s.Panes(ctx)
			if err != nil {
				continue
			}
			for _, p := range panes {
				if len(p.Options) == 0 {
					continue
				}
				if had, err := attach.Clear(ctx, a.log.Dir, s, p.ID); err == nil && had {
					cleared++
				}
			}
			// Left behind only by a tmag serve that was killed.
			_, _ = s.Run(ctx, "set-option", "-gu", serveAddressOption)
		}
		if err := removeState(auth.DefaultDir()); err != nil {
			return a.fail("uninstall", err)
		}
		fmt.Fprintf(a.stdout, "cleared tmag options from %d pane(s) and removed tmag's state from %s, keeping its lock files for hooks that run meanwhile\n", cleared, a.log.Dir)
		return ExitOK
	}
	self, _ := os.Executable()
	removals := []string{
		fmt.Sprintf("claude plugin uninstall %s && claude plugin uninstall %s && claude plugin marketplace remove %s", pluginID(peersName), pluginID(pluginName), marketplace),
		fmt.Sprintf("codex plugin remove %s && codex plugin remove %s && codex plugin marketplace remove %s", pluginID(peersName), pluginID(pluginName), marketplace),
	}
	// pi remove takes a package's path: those Pi's settings list, else the
	// tmux plugin's.
	paths := tmagPiPackages(ctx)
	if root := pluginRoot(os.Getenv); len(paths) == 0 && root != "" {
		dir := filepath.Join(root, "plugins", "pi")
		paths = []string{filepath.Join(dir, peersName), filepath.Join(dir, pluginName)}
	}
	if len(paths) > 0 {
		removals = append(removals, "pi remove "+strings.Join(paths, " && pi remove "))
	}
	removals = append(removals, fmt.Sprintf("omp plugin uninstall %s && omp plugin uninstall %s", peersName, pluginName))
	fmt.Fprintf(a.stdout, `Remove tmag in this order. Running agents and tmux sessions are not affected.
Close tmag serve's window first, if it runs.

1. tmag's state (pane options and %[1]s), while the binary still exists:
     tmag uninstall --purge
2. The agent plugins, before the tmux plugin that Pi's and oh-my-pi's come
   from (skip tmag-peers where it is not installed):
     %[2]s
3. The tmux plugin and its binary: delete this line from tmux.conf, then press
   prefix + alt + u (TPM's clean) to remove the plugin directory
     set -g @plugin '%[3]s'
`, a.log.Dir, strings.Join(removals, "\n     "), repo)
	if !inPluginDir(canonical(self)) {
		fmt.Fprintf(a.stdout, "4. This binary, installed outside the tmux plugin:\n     rm %s\n", self)
	}
	return ExitOK
}
