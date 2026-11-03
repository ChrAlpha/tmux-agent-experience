// Package binpath locates the tmag binary the way agent hooks do.
//
// The tmux plugin keeps its binary inside its own TPM directory,
// $TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/bin/tmag. Agents started in
// tmux inherit TMUX_PLUGIN_MANAGER_PATH from tmux's global environment; a
// process started elsewhere (a Codex daemon, say) falls back to TPM's two
// default locations, and finally to tmag on PATH.
package binpath

import "strings"

// PluginDir is the plugin's directory name under the TPM plugin path.
const PluginDir = "tmux-agent-experience"

// Getenv looks up an environment variable.
type Getenv func(string) string

// pluginRoots are the TPM plugin directories hooks try, in order, written as
// shell words. HookCommand and PluginCandidates must agree on them.
func pluginRoots(getenv Getenv) []string {
	xdg := getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = getenv("HOME") + "/.config"
	}
	return []string{
		strings.TrimSuffix(getenv("TMUX_PLUGIN_MANAGER_PATH"), "/"),
		getenv("HOME") + "/.tmux/plugins",
		xdg + "/tmux/plugins",
	}
}

// PluginCandidates lists where the tmux plugin's binary may be, in hook order.
// Only absolute roots count: an unset or relative variable must never make a
// hook run a file from the agent's working directory.
func PluginCandidates(getenv Getenv) []string {
	var out []string
	for _, root := range pluginRoots(getenv) {
		if !strings.HasPrefix(root, "/") {
			continue
		}
		p := root + "/" + PluginDir + "/bin/tmag"
		dup := false
		for _, seen := range out {
			dup = dup || seen == p
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// HookCommand is the POSIX shell command an agent plugin runs for every hook.
// It tries the plugin's binary in the roots of PluginCandidates, then tmag on
// PATH, and exits 0 when none exists, so a missing tmag never disturbs the agent.
func HookCommand(agent string) string { return command("hook " + agent) }

// AnswerHookCommand is the command Claude Code's plugin runs for
// PermissionRequest instead: `tmag answer-hook`, found the same way.
func AnswerHookCommand(agent string) string { return command("answer-hook " + agent) }

func command(args string) string {
	return `for r in "${TMUX_PLUGIN_MANAGER_PATH%/}" "$HOME/.tmux/plugins" "${XDG_CONFIG_HOME:-$HOME/.config}/tmux/plugins";` +
		` do case $r in /*) t="$r/` + PluginDir + `/bin/tmag"; [ -x "$t" ] && exec "$t" ` + args + ` 2>/dev/null;; esac; done;` +
		` t=$(command -v tmag) && exec "$t" ` + args + ` 2>/dev/null; exit 0`
}
