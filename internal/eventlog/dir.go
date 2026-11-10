package eventlog

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Layout names how this tmag lays out its state directory. The default
// directory is the layout's own, so tmags of different layouts never share
// one; a directory TMAG_STATE_DIR names serves one layout only.
const Layout = "v2"

// DefaultDir resolves the state directory: $TMAG_STATE_DIR as it is, or
// else the directory of this process's realm (proc.Realm) under Base and
// Layout. Each realm has its own because tmag proves sessions from the
// process table it sees: two containers sharing a home would otherwise end
// each other's sessions, whose processes neither can see.
func DefaultDir() string {
	if d := os.Getenv("TMAG_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(Base(), Layout, proc.Realm())
}

// DefaultDirContext includes realm discovery in the caller's operation budget.
func DefaultDirContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if d := os.Getenv("TMAG_STATE_DIR"); d != "" {
		return d, nil
	}
	realm, err := proc.RealmContext(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(Base(), Layout, realm), nil
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
