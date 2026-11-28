package driver

import (
	"slices"
	"testing"
)

func TestSendPlan(t *testing.T) {
	inbox, pane, svc := []string{Inbox}, []string{Pane}, []string{Service}
	for _, c := range []struct {
		name           string
		agent, runtime string
		caller         Caller
		wait, idle     bool
		want           Plan
	}{
		{"a person's idle Claude prompt is typed", "claude", "terminal", Person, false, true, Plan{Try: pane}},
		{"a person's working Claude prompt goes to its inbox", "claude", "terminal", Person, false, false, Plan{Try: inbox, Busy: true}},
		{"a person's Claude wait stays on the pane", "claude", "terminal", Person, true, true, Plan{Try: pane}},
		{"a person's working Claude wait stays on the pane", "claude", "terminal", Person, true, false, Plan{Try: pane, Busy: true}},
		{"an agent's idle Claude prompt tries its inbox, then the pane", "claude", "terminal", Agent, false, true, Plan{Try: []string{Inbox, Pane}}},
		{"an agent's working Claude prompt goes to its inbox", "claude", "terminal", Agent, false, false, Plan{Try: inbox, Busy: true}},
		{"an agent's idle Claude wait is an inbox turn, or the pane", "claude", "terminal", Agent, true, true, Plan{Try: []string{Inbox, Pane}}},
		{"a person's background Claude prompt goes to its inbox, never busy", "claude", "service", Person, false, false, Plan{Try: inbox}},
		{"a person's background Claude wait has no pane", "claude", "service", Person, true, true, Plan{}},
		{"an agent's background Claude wait is an inbox turn", "claude", "service", Agent, true, true, Plan{Try: inbox}},
		{"a person's idle Codex prompt is typed", "codex", "terminal", Person, false, true, Plan{Try: pane}},
		{"a working Codex in a pane is busy", "codex", "terminal", Agent, false, false, Plan{Busy: true}},
		{"the Codex daemon takes every prompt", "codex", "service", Person, false, true, Plan{Try: svc}},
		{"the Codex daemon takes an agent's wait", "codex", "service", Agent, true, false, Plan{Try: svc}},
		{"a person's working Pi prompt goes to its inbox", "pi", "terminal", Person, false, false, Plan{Try: inbox, Busy: true}},
		{"an agent's idle oh-my-pi prompt tries its inbox, then the pane", "omp", "terminal", Agent, false, true, Plan{Try: []string{Inbox, Pane}}},
		{"Pi serving a program takes no prompt", "pi", "service", Agent, false, true, Plan{}},
		{"oh-my-pi serving a program takes no prompt", "omp", "service", Person, false, false, Plan{}},
		{"a person's idle declared prompt goes to its inbox", "gemini", "terminal", Person, false, true, Plan{Try: inbox}},
		{"a person's declared wait goes to its inbox", "gemini", "terminal", Person, true, true, Plan{Try: inbox}},
		{"an agent's working declared prompt goes to its inbox, never busy", "gemini", "terminal", Agent, false, false, Plan{Try: inbox}},
		{"a declared agent serving a program takes no prompt", "gemini", "service", Agent, false, true, Plan{}},
		{"a program that names no agent", "bash", "terminal", Person, false, true, Plan{}},
	} {
		got := SendPlan(c.agent, c.runtime, c.caller, c.wait, c.idle)
		if !slices.Equal(got.Try, c.want.Try) || got.Busy != c.want.Busy {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestInterruptPlanAndAnswerChannel(t *testing.T) {
	for _, c := range []struct {
		agent, runtime string
		interrupt      []string
		answer         string
	}{
		{"claude", "terminal", []string{Pane}, Hook},
		{"claude", "service", nil, Hook},
		{"codex", "terminal", []string{Pane}, ""},
		{"codex", "service", []string{Service}, Service},
		{"pi", "terminal", []string{Inbox, Pane}, ""},
		{"pi", "service", nil, ""},
		{"omp", "terminal", []string{Pane}, ""},
		{"omp", "service", nil, ""},
		{"gemini", "terminal", []string{Inbox}, ""},
		{"gemini", "service", nil, ""},
		{"bash", "terminal", nil, ""},
	} {
		if got := InterruptPlan(c.agent, c.runtime); !slices.Equal(got.Try, c.interrupt) || got.Busy {
			t.Errorf("InterruptPlan(%s, %s) = %+v, want %v", c.agent, c.runtime, got, c.interrupt)
		}
		if got := AnswerChannel(c.agent, c.runtime); got != c.answer {
			t.Errorf("AnswerChannel(%s, %s) = %q, want %q", c.agent, c.runtime, got, c.answer)
		}
	}
}

func TestTurnsChannel(t *testing.T) {
	for _, c := range []struct {
		agent, runtime, want string
	}{
		{"gemini", "terminal", Inbox},
		{"gemini", "service", ""},
		{"claude", "terminal", ""},
		{"pi", "terminal", ""},
	} {
		if got := TurnsChannel(c.agent, c.runtime); got != c.want {
			t.Errorf("TurnsChannel(%s, %s) = %q, want %q", c.agent, c.runtime, got, c.want)
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
		{"gemini", "terminal", []string{Send, Interrupt}},
		{"gemini", "service", nil},
	} {
		got := Capabilities(c.agent, c.runtime)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %s takes %v, want %v", c.agent, c.runtime, got, c.want)
		}
	}
	for agent, reports := range map[string]bool{"claude": false, "codex": true, "pi": true, "omp": true, "gemini": true} {
		if ReportsInterrupt(agent) != reports {
			t.Errorf("ReportsInterrupt(%s) = %v", agent, !reports)
		}
	}
}
