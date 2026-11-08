package eventlog

import (
	"os"
	"path/filepath"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// DefaultDir resolves the state directory: $TMAG_STATE_DIR as it is, or
// else the directory of this process's realm (proc.Realm) under Base. Each
// realm has its own because tmag proves sessions from the process table it
// sees: two containers sharing a home would otherwise end each other's
// sessions, whose processes neither can see.
func DefaultDir() string {
	if d := os.Getenv("TMAG_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(Base(), proc.Realm())
}

// Base is where the realms' state directories are: $XDG_STATE_HOME/tmag,
// else ~/.local/state/tmag.
func Base() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tmag")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "state", "tmag")
}
