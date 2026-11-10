package driver

import (
	"slices"
	"testing"
)

func TestChannel(t *testing.T) {
	for _, c := range []struct{ agent, runtime, verb, want string }{
		{"claude", "terminal", Interrupt, Pane},
		{"claude", "terminal", Answer, Hook},
		{"claude", "terminal", Send, ""},
		{"claude", "service", Interrupt, ""},
		{"claude", "service", Answer, Hook},
		{"claude", "service", Send, ""},
		{"codex", "terminal", Interrupt, Pane},
		{"codex", "terminal", Answer, ""},
		{"codex", "service", Interrupt, Service},
		{"codex", "service", Answer, Service},
		{"pi", "terminal", Interrupt, Pane},
		{"pi", "terminal", Answer, ""},
		{"pi", "service", Interrupt, ""},
		{"pi", "service", Answer, ""},
		{"aider", "terminal", Interrupt, ""},
	} {
		if got := Channel(c.agent, c.runtime, c.verb); got != c.want {
			t.Errorf("Channel(%s, %s, %s) = %q, want %q", c.agent, c.runtime, c.verb, got, c.want)
		}
	}
}

func TestSendRoute(t *testing.T) {
	for _, c := range []struct {
		name           string
		agent, runtime string
		caller         Caller
		mode           Mode
		idle           bool
		want           Route
	}{
		{"a person's idle Claude prompt is typed", "claude", "terminal", Person, Now, true, Route{Channel: Pane}},
		{"a person's working Claude prompt goes to its inbox", "claude", "terminal", Person, Now, false, Route{Channel: Inbox}},
		{"a person's working Claude reply goes to its inbox", "claude", "terminal", Person, Reply, false, Route{Channel: Inbox}},
		{"a person's Claude wait stays on the pane", "claude", "terminal", Person, Wait, true, Route{Channel: Pane}},
		{"a person's working Claude wait stays on the pane", "claude", "terminal", Person, Wait, false, Route{Channel: Pane}},
		{"an agent's idle Claude prompt goes to its inbox", "claude", "terminal", Agent, Now, true, Route{Channel: Inbox, Fallback: Pane}},
		{"an agent's working Claude prompt goes to its inbox", "claude", "terminal", Agent, Now, false, Route{Channel: Inbox}},
		{"an agent's idle Claude wait is an inbox turn", "claude", "terminal", Agent, Wait, true, Route{Channel: Inbox, Fallback: Pane}},
		{"a person's Claude background prompt goes to its inbox", "claude", "service", Person, Now, true, Route{Channel: Inbox}},
		{"a person's Claude background wait has no pane", "claude", "service", Person, Wait, true, Route{}},
		{"an agent's Claude background wait is an inbox turn", "claude", "service", Agent, Wait, true, Route{Channel: Inbox}},
		{"a person's idle Codex prompt is typed", "codex", "terminal", Person, Now, true, Route{Channel: Pane}},
		{"a person's working Codex prompt has no channel", "codex", "terminal", Person, Now, false, Route{}},
		{"an agent's idle Codex prompt is typed", "codex", "terminal", Agent, Now, true, Route{Channel: Pane}},
		{"an agent's working Codex prompt has no channel", "codex", "terminal", Agent, Now, false, Route{}},
		{"the Codex daemon takes every prompt", "codex", "service", Person, Now, true, Route{Channel: Service}},
		{"the Codex daemon takes an agent's wait", "codex", "service", Agent, Wait, false, Route{Channel: Service}},
		{"a person's idle Pi prompt is typed", "pi", "terminal", Person, Now, true, Route{Channel: Pane}},
		{"a person's working Pi prompt goes to its inbox", "pi", "terminal", Person, Now, false, Route{Channel: Inbox}},
		{"a person's Pi wait stays on the pane", "pi", "terminal", Person, Wait, true, Route{Channel: Pane}},
		{"an agent's idle Pi prompt goes to its inbox", "pi", "terminal", Agent, Now, true, Route{Channel: Inbox, Fallback: Pane}},
		{"an agent's working oh-my-pi prompt goes to its inbox", "omp", "terminal", Agent, Reply, false, Route{Channel: Inbox}},
		{"Pi serving a program takes no prompt", "pi", "service", Agent, Now, true, Route{}},
		{"oh-my-pi serving a program takes no prompt", "omp", "service", Person, Now, true, Route{}},
	} {
		if got := SendRoute(c.agent, c.runtime, c.caller, c.mode, c.idle); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	for _, c := range []struct {
		agent, runtime string
		want           []string
	}{
		{"claude", "terminal", []string{Send, Interrupt, Answer}},
		{"claude", "service", []string{Send, Answer}},
		{"codex", "terminal", []string{Send, Interrupt}},
		{"codex", "service", []string{Send, Interrupt, Answer}},
		{"pi", "terminal", []string{Send, Interrupt}},
		{"pi", "service", nil},
		{"omp", "terminal", []string{Send, Interrupt}},
		{"omp", "service", nil},
	} {
		got := Capabilities(c.agent, c.runtime)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %s takes %v, want %v", c.agent, c.runtime, got, c.want)
		}
	}
	for agent, reports := range map[string]bool{"claude": false, "codex": true, "pi": true, "omp": true} {
		if ReportsInterrupt(agent) != reports {
			t.Errorf("ReportsInterrupt(%s) = %v", agent, !reports)
		}
	}
}
