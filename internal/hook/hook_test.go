package hook

import (
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestAlert(t *testing.T) {
	for _, c := range []struct{ prev, next, want string }{
		{"working", "blocked", "blocked"},
		{"", "blocked", "blocked"},
		{"blocked", "blocked", ""},
		{"working", "idle", "finished"},
		{"blocked", "working", ""},
		{"idle", "idle", ""},
		{"", "idle", ""},
	} {
		if got := Alert(c.prev, c.next); got != c.want {
			t.Errorf("Alert(%q, %q) = %q, want %q", c.prev, c.next, got, c.want)
		}
	}
}

// A hook reconciles at a session's start and end, and when it changes how
// the session's host runs it; a session's first host, or the same one,
// keeps tool hooks off the full scan.
func TestReconciles(t *testing.T) {
	terminal := session.Host{PID: 7, Start: "s", Role: proc.Terminal}
	service := session.Host{PID: 7, Start: "s", Role: proc.Service}
	tool := adapter.Payload{HookEventName: "PreToolUse"}
	for _, c := range []struct {
		name       string
		p          adapter.Payload
		prev, next session.Host
		want       bool
	}{
		{"a session's start", adapter.Payload{HookEventName: "SessionStart"}, terminal, terminal, true},
		{"its first host", tool, session.Host{}, terminal, false},
		{"the same host", tool, terminal, terminal, false},
		{"a host that turns to serve a program", tool, terminal, service, true},
		{"one that turns back", tool, service, terminal, true},
	} {
		prev, next := session.Record{ID: "s1", Host: c.prev}, session.Record{ID: "s1", Host: c.next}
		if got := reconciles(c.p, prev, next); got != c.want {
			t.Errorf("%s: reconciles = %v", c.name, got)
		}
	}
}
