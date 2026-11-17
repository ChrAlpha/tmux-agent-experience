package hook

import (
	"context"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestHintValuesWithTmuxSyntaxAreRefused(t *testing.T) {
	for _, h := range []session.PaneHint{
		{Server: "/tmp/tmux.sock", ID: "%1", PID: 10, Start: "x}#{run-shell,touch /tmp/pwn}"},
		{Server: "/tmp/tmux.sock", ID: "%x", PID: 10, Start: "10"},
		{Server: "/tmp/tmux.sock", ID: "%1", PID: 0, Start: "10"},
		{Server: "/tmp/tmux.sock", ID: "%1", PID: 10, Start: ""},
	} {
		if _, _, _, _, _, ok := writeHintProjection(context.Background(), h, "claude", "session", attach.Projection{State: "working"}, false); ok {
			t.Fatalf("hint %+v was used", h)
		}
	}
	if _, _, _, _, _, ok := writeHintProjection(context.Background(),
		session.PaneHint{Server: "/tmp/tmux.sock", ID: "%1", PID: 10, Start: "10"},
		"claude", "bad id #{}", attach.Projection{State: "working"}, false); ok {
		t.Fatal("a session id with tmux syntax was used")
	}
}
