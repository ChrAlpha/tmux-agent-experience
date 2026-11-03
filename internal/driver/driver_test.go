package driver

import (
	"slices"
	"testing"
)

func TestChannel(t *testing.T) {
	for _, c := range []struct{ agent, runtime, verb, want string }{
		{"claude", "terminal", Send, Pane},
		{"claude", "", Send, Pane},
		{"claude", "service", Send, ""},
		{"codex", "terminal", Send, Pane},
		{"codex", "service", Send, Pane},
		{"aider", "terminal", Send, ""},
	} {
		if got := Channel(c.agent, c.runtime, c.verb); got != c.want {
			t.Errorf("Channel(%s, %s, %s) = %q, want %q", c.agent, c.runtime, c.verb, got, c.want)
		}
	}
	if got := Capabilities("claude", "service"); !slices.Equal(got, []string{}) {
		t.Errorf("a Claude background session takes %v", got)
	}
}
