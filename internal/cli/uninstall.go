package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func (a *app) cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("uninstall", err)
	}
	if *purge {
		cleared := 0
		ctx := context.Background()
		for _, s := range tmux.Servers() {
			panes, err := s.Panes(ctx)
			if err != nil {
				continue
			}
			for _, p := range panes {
				if len(p.Options) > 0 && s.ClearPaneOptions(ctx, p.ID) == nil {
					cleared++
				}
			}
			// Left behind only by a tmag serve that was killed.
			_, _ = s.Run(ctx, "set-option", "-gu", notify.URLOption)
		}
		if err := os.RemoveAll(a.log.Dir); err != nil {
			return a.fail("uninstall", err)
		}
		if filepath.Dir(filepath.Clean(a.log.Dir)) == filepath.Clean(eventlog.Base()) {
			_ = os.Remove(eventlog.Base()) // once no other realm keeps state there
		}
		fmt.Fprintf(a.stdout, "cleared tmag options from %d pane(s) and removed %s\n", cleared, a.log.Dir)
		return ExitOK
	}
	self, _ := os.Executable()
	fmt.Fprintf(a.stdout, `Remove tmag in this order. Running agents and tmux sessions are not affected.
Close tmag serve's window first, if it runs.

1. tmag's state (pane options and %[1]s), while the binary still exists:
     tmag uninstall --purge
2. The agent plugins (skip tmag-peers where it is not installed):
     claude plugin uninstall %[5]s && claude plugin uninstall %[2]s && claude plugin marketplace remove %[3]s
     codex plugin remove %[5]s && codex plugin remove %[2]s && codex plugin marketplace remove %[3]s
3. The tmux plugin and its binary: delete this line from tmux.conf, then press
   prefix + alt + u (TPM's clean) to remove the plugin directory
     set -g @plugin '%[4]s'
`, a.log.Dir, pluginID(pluginName), marketplace, repo, pluginID(peersName))
	if !inPluginDir(canonical(self)) {
		fmt.Fprintf(a.stdout, "4. This binary, installed outside the tmux plugin:\n     %s\n", removeBinary(self))
	}
	return ExitOK
}

// removeBinary names the command that removes a tmag installed outside the
// tmux plugin, the way it was installed.
func removeBinary(self string) string {
	if inMiseInstalls(canonical(self)) {
		return "mise unuse -g " + miseTool
	}
	return "rm " + self
}
