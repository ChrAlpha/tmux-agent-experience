package eventlog

import (
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

func TestDefaultDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv("TMAG_STATE_DIR", "")
	if got, want := DefaultDir(), filepath.Join(xdg, "tmag", proc.Realm()); got != want {
		t.Errorf("DefaultDir() = %q, want the realm's directory %q", got, want)
	}
	t.Setenv("TMAG_STATE_DIR", "/scratch/state")
	if got := DefaultDir(); got != "/scratch/state" {
		t.Errorf("DefaultDir() = %q, want $TMAG_STATE_DIR as it is", got)
	}
}
