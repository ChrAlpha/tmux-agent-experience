package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// shownIn is a session in state, shown by pane, that has been in that state
// for waited; pane "" shows nothing.
func shownIn(v *query.View, pane, state string, waited time.Duration) {
	since := time.Now().Add(-waited)
	id := "id" + pane + state
	s := query.Session{ID: id, Agent: "claude", State: state, Since: &since, Panes: []query.Shown{}}
	p := query.Pane{ID: pane, Agent: "claude", Session: id}
	if pane != "" {
		s.Panes = append(s.Panes, query.Shown{ID: pane})
		v.Panes = append(v.Panes, p)
	}
	v.Sessions = append(v.Sessions, s)
}

func TestSummary(t *testing.T) {
	var v query.View
	shownIn(&v, "%1", "blocked", 0)
	shownIn(&v, "%2", "blocked", 0)
	shownIn(&v, "%3", "working", 0)
	shownIn(&v, "%4", "unknown", 0)
	if got := Summary(v.Sessions, false); got != "2 blocked · 1 working" {
		t.Fatalf("plain = %q", got)
	}
	if got := Summary(v.Sessions, true); !strings.HasPrefix(got, "#[fg=red,bold]2 blocked#[default]") {
		t.Fatalf("styled = %q", got)
	}
	if got := Summary(nil, true); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestWaiting(t *testing.T) {
	var v query.View
	shownIn(&v, "%1", "idle", time.Hour)
	shownIn(&v, "%2", "blocked", time.Minute)
	shownIn(&v, "%3", "blocked", 10*time.Minute)
	shownIn(&v, "%4", "working", 2*time.Hour)
	shownIn(&v, "", "blocked", 3*time.Hour) // no pane to jump to
	v.Panes = append(v.Panes, query.Pane{ID: "%5", Agent: "codex"})
	pick := func(v query.View, from string) string {
		p, ok := Waiting(v, from)
		if !ok {
			return ""
		}
		return p.ID
	}
	if got := pick(v, ""); got != "%3" {
		t.Fatalf("picked %s, want the longest-blocked %%3", got)
	}
	if got := pick(v, "%3"); got != "%2" {
		t.Fatalf("from %%3 picked %s", got)
	}
	var idle query.View
	shownIn(&idle, "%1", "idle", time.Hour)
	shownIn(&idle, "%4", "working", 2*time.Hour)
	if got := pick(idle, ""); got != "%1" {
		t.Fatalf("idle fallback picked %s", got)
	}
	if got := pick(idle, "%1"); got != "" {
		t.Fatal("picked the pane the user is in")
	}
}

func TestSetupPlan(t *testing.T) {
	first := func(plan [][]string) string {
		if len(plan) == 0 {
			return ""
		}
		return strings.Join(plan[0], " ")
	}
	for _, c := range []struct {
		name string
		h    host
		want string
		n    int
	}{
		{"absent", host{CLI: "claude"}, "", 0},
		{"unlistable", host{CLI: "claude", Present: true}, "", 0},
		{"claude install", host{CLI: "claude", Present: true, Listed: true}, "claude plugin marketplace add " + repo + " --sparse .claude-plugin plugins/claude", 2},
		{"claude current", host{CLI: "claude", Present: true, Listed: true, Installed: true, Enabled: true, PluginVer: version.Version}, "", 0},
		{"claude disabled", host{CLI: "claude", Present: true, Listed: true, Installed: true, PluginVer: version.Version}, "claude plugin enable " + pluginID, 1},
		{"claude disabled and outdated", host{CLI: "claude", Present: true, Listed: true, Installed: true, PluginVer: "0.0.1"}, "claude plugin marketplace update " + marketplace, 3},
		{"claude outdated", host{CLI: "claude", Present: true, Listed: true, Installed: true, Enabled: true, PluginVer: "0.0.1"}, "claude plugin marketplace update " + marketplace, 2},
		{"codex install", host{CLI: "codex", Present: true, Listed: true}, "codex plugin marketplace add " + repo + " --sparse .agents --sparse plugins/codex", 2},
		{"codex outdated", host{CLI: "codex", Present: true, Listed: true, Installed: true, Enabled: true, PluginVer: "0.0.1"}, "codex plugin marketplace upgrade " + marketplace, 2},
	} {
		plan := setupPlan(c.h)
		if first(plan) != c.want || len(plan) != c.n {
			t.Errorf("%s: plan %v", c.name, plan)
		}
	}
	plan := setupPlan(host{CLI: "claude", Present: true, Listed: true, Installed: true, PluginVer: "0.0.1"})
	if !reflect.DeepEqual(plan[2], []string{"claude", "plugin", "enable", pluginID}) {
		t.Errorf("disabled outdated claude plugin ends with %v", plan[2])
	}
	plan = setupPlan(host{CLI: "codex", Present: true, Listed: true})
	if !reflect.DeepEqual(plan[1], []string{"codex", "plugin", "add", pluginID}) {
		t.Errorf("codex install ends with %v", plan[1])
	}
}
