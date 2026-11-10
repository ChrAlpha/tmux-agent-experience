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
		{"codex", "service", Send, Service},
		{"codex", "service", Interrupt, Service},
		{"aider", "terminal", Send, ""},
		{"claude", "terminal", Interrupt, Pane},
		{"claude", "service", Interrupt, ""},
		{"codex", "terminal", Interrupt, Pane},
		{"claude", "terminal", Answer, Hook},
		{"claude", "service", Answer, Hook},
		{"codex", "terminal", Answer, ""},
		{"codex", "service", Answer, Service},
		{"pi", "terminal", Send, Pane},
		{"pi", "terminal", Interrupt, Pane},
		{"pi", "terminal", Answer, ""},
		{"pi", "service", Send, ""},
		{"omp", "terminal", Send, Pane},
		{"omp", "", Interrupt, Pane},
		{"omp", "terminal", Answer, ""},
		{"omp", "service", Interrupt, ""},
	} {
		if got := Channel(c.agent, c.runtime, c.verb); got != c.want {
			t.Errorf("Channel(%s, %s, %s) = %q, want %q", c.agent, c.runtime, c.verb, got, c.want)
		}
	}
	if got := Capabilities("claude", "service"); !slices.Equal(got, []string{Answer}) {
		t.Errorf("a Claude background session takes %v", got)
	}
	if got := Capabilities("pi", "service"); len(got) != 0 {
		t.Errorf("Pi serving a program takes %v", got)
	}
	for agent, reports := range map[string]bool{"claude": false, "codex": true, "pi": true, "omp": true} {
		if ReportsInterrupt(agent) != reports {
			t.Errorf("ReportsInterrupt(%s) = %v", agent, !reports)
		}
	}
}
