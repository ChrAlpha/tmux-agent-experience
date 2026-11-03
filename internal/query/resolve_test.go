package query

import (
	"errors"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestResolve(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1/work,1,0")
	t.Setenv("TMUX_TMPDIR", "/tmp")
	agents := []Agent{
		{ID: "abcd1111", Pane: PaneInfo{Server: "/tmp/tmux-1/work", ID: "%1"}},
		{ID: "abcd2222", Pane: PaneInfo{Server: "/tmp/tmux-1/work", ID: "%2"}},
		{ID: "", Binding: "unbound", Pane: PaneInfo{Server: tmux.ServerByName("other").Socket, ID: "%1"}},
	}
	ok := func(target, wantPane, wantServer string) {
		t.Helper()
		a, err := Resolve(agents, target)
		if err != nil || a.Pane.ID != wantPane || a.Pane.Server != wantServer {
			t.Fatalf("Resolve(%q) = %+v, %v", target, a.Pane, err)
		}
	}
	ok("abcd2", "%2", "/tmp/tmux-1/work")
	ok("%1", "%1", "/tmp/tmux-1/work")
	ok("other:%1", "%1", tmux.ServerByName("other").Socket)
	ok("/tmp/tmux-1/work:%2", "%2", "/tmp/tmux-1/work")

	for target, want := range map[string]error{
		"abcd": ErrAmbiguous, "abc": ErrNotFound, "ffff": ErrNotFound, "%9": ErrNotFound,
	} {
		if _, err := Resolve(agents, target); !errors.Is(err, want) {
			t.Errorf("Resolve(%q) err = %v, want %v", target, err, want)
		}
	}
}
