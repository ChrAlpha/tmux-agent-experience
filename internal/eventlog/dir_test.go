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
	realm, err := proc.Realm(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DefaultDir(t.Context()); err != nil || got != filepath.Join(xdg, "tmag", Layout, realm) {
		t.Errorf("DefaultDir = %q, %v, want the realm's directory of this layout", got, err)
	}
	t.Setenv("TMAG_STATE_DIR", "/scratch/state")
	if got, err := DefaultDir(t.Context()); err != nil || got != "/scratch/state" {
		t.Errorf("DefaultDir = %q, %v, want $TMAG_STATE_DIR as it is", got, err)
	}
}
