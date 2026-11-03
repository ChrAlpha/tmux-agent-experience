package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

func agent(pane, state, binding string, waited time.Duration) query.Agent {
	since := time.Now().Add(-waited)
	return query.Agent{ID: "id" + pane, State: state, Binding: binding, Since: &since, Pane: query.PaneInfo{ID: pane}}
}

func TestSummary(t *testing.T) {
	agents := []query.Agent{
		agent("%1", "blocked", "process", 0), agent("%2", "blocked", "process", 0),
		agent("%3", "working", "process", 0), agent("%4", "unknown", "unbound", 0),
	}
	if got := Summary(agents, false); got != "2 blocked · 1 working" {
		t.Fatalf("plain = %q", got)
	}
	if got := Summary(agents, true); !strings.HasPrefix(got, "#[fg=red,bold]2 blocked#[default]") {
		t.Fatalf("styled = %q", got)
	}
	if got := Summary(nil, true); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestWaiting(t *testing.T) {
	agents := []query.Agent{
		agent("%1", "idle", "process", time.Hour),
		agent("%2", "blocked", "process", time.Minute),
		agent("%3", "blocked", "process", 10*time.Minute),
		agent("%4", "working", "process", 2*time.Hour),
		agent("%5", "unknown", "unbound", 3*time.Hour),
	}
	pick := func(from string) string {
		a, ok := Waiting(agents, from)
		if !ok {
			return ""
		}
		return a.Pane.ID
	}
	if got := pick(""); got != "%3" {
		t.Fatalf("picked %s, want the longest-blocked %%3", got)
	}
	if got := pick("%3"); got != "%2" {
		t.Fatalf("from %%3 picked %s", got)
	}
	if got := pick("%x"); got != "%3" {
		t.Fatalf("picked %s", got)
	}
	idleOnly := []query.Agent{agents[0], agents[3]}
	if a, ok := Waiting(idleOnly, ""); !ok || a.Pane.ID != "%1" {
		t.Fatalf("idle fallback picked %+v", a)
	}
	if _, ok := Waiting(idleOnly, "%1"); ok {
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
