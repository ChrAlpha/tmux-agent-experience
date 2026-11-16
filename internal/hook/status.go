package hook

import (
	"path/filepath"
	"strconv"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// claudeConfigVar names Claude Code's configuration directory when the person
// moved it; the session status files live under its sessions directory.
const claudeConfigVar = "CLAUDE_CONFIG_DIR"

// statusFileOf is where Claude Code writes the status of the session hostPID:
// $CLAUDE_CONFIG_DIR/sessions/<pid>.json when that directory is set, else
// $HOME/.claude/sessions/<pid>.json. Only an absolute directory names a file
// tmag may read, and only Claude Code writes one.
func statusFileOf(agent string, hostPID int, getenv func(string) string) string {
	if agent != proc.Claude || hostPID <= 0 {
		return ""
	}
	dir := getenv(claudeConfigVar)
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	path := filepath.Join(dir, "sessions", strconv.Itoa(hostPID)+".json")
	if len(path) > adapter.MaxPath {
		return ""
	}
	return path
}
