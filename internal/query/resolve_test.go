package query

import (
	"context"
	"errors"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestResolve(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1/work,1,0")
	t.Setenv("TMUX_TMPDIR", "/tmp")
	other := tmux.ServerByName("other").Socket
	v := View{
		Sessions: []Session{
			{ID: "abcd1111", Agent: "claude", Panes: []Shown{{Server: "/tmp/tmux-1/work", ID: "%1"}}},
			{ID: "abcd2222", Agent: "codex", Panes: []Shown{{Server: "/tmp/tmux-1/work", ID: "%2"}, {Server: other, ID: "%2"}}},
			{ID: "ffff0000", Agent: "codex"},
		},
		Panes: []Pane{
			{Server: "/tmp/tmux-1/work", ID: "%1", Agent: "claude", Session: "abcd1111"},
			{Server: "/tmp/tmux-1/work", ID: "%2", Agent: "codex", Session: "abcd2222"},
			{Server: other, ID: "%1", Agent: "codex"},
			{Server: other, ID: "%2", Agent: "codex", Session: "abcd2222"},
		},
	}
	ok := func(target, session, pane, server string) {
		t.Helper()
		got, err := Resolve(context.Background(), v, target)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", target, err)
		}
		if (got.Session == nil) != (session == "") || (got.Session != nil && got.Session.ID != session) {
			t.Fatalf("Resolve(%q) session = %+v, want %q", target, got.Session, session)
		}
		if (got.Pane == nil) != (pane == "") || (got.Pane != nil && (got.Pane.ID != pane || got.Pane.Server != server)) {
			t.Fatalf("Resolve(%q) pane = %+v, want %s on %s", target, got.Pane, pane, server)
		}
	}
	ok("abcd1", "abcd1111", "%1", "/tmp/tmux-1/work") // a session shown once resolves to its pane
	ok("abcd2", "abcd2222", "", "")                   // shown twice: the session only
	ok("ffff", "ffff0000", "", "")                    // shown nowhere
	ok("%1", "abcd1111", "%1", "/tmp/tmux-1/work")    // a pane, and what it shows
	ok("other:%1", "", "%1", other)                   // a pane that shows nothing provable
	ok("other:%2", "abcd2222", "%2", other)           // one of two panes of a session
	ok("/tmp/tmux-1/work:%2", "abcd2222", "%2", "/tmp/tmux-1/work")

	for target, want := range map[string]error{
		"abcd": ErrAmbiguous, "abc": ErrNotFound, "eeee": ErrNotFound, "%9": ErrNotFound,
	} {
		if _, err := Resolve(context.Background(), v, target); !errors.Is(err, want) {
			t.Errorf("Resolve(%q) err = %v, want %v", target, err, want)
		}
	}
}

// A name is what has none of tmux's syntax: tmag matches it exactly, and
// leaves the rest to tmux.
func TestIsName(t *testing.T) {
	for target, want := range map[string]bool{
		"w-1": true, "reviewer": true, "ws6_pi": true, "a b": true, "2nd": true,
		"": false, "7": false, "main:1": false, "main:w-1.0": false, "%3": false, "@2": false,
		"$1": false, "=w-1": false, "{last}": false, "!": false, "^": false, "+": false, "-": false, "~": false,
	} {
		if got := isName(target); got != want {
			t.Errorf("isName(%q) = %v, want %v", target, got, want)
		}
	}
}
