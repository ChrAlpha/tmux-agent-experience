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
	cur := plugin{Installed: true, Enabled: true, Version: version.Version}
	old := plugin{Installed: true, Enabled: true, Version: "0.0.1"}
	off := plugin{Installed: true, Version: version.Version}
	claude := func(tmag, peers plugin) host {
		return host{CLI: "claude", Present: true, Listed: true, Tmag: tmag, Peers: peers}
	}
	codex := func(tmag, peers plugin) host {
		return host{CLI: "codex", Present: true, Listed: true, Tmag: tmag, Peers: peers}
	}
	pi := func(tmag, peers plugin) host {
		return host{CLI: "pi", Present: true, Listed: true, Packages: "/p/plugins/pi", Tmag: tmag, Peers: peers}
	}
	omp := func(tmag, peers plugin) host {
		return host{CLI: "omp", Present: true, Listed: true, Packages: "/p/plugins/omp", Tmag: tmag, Peers: peers}
	}
	cm, xm := "claude plugin marketplace ", "codex plugin marketplace "
	ci, xi := "claude plugin ", "codex plugin "
	tmag, peers := pluginID(pluginName), pluginID(peersName)
	for _, c := range []struct {
		name  string
		h     host
		peers bool
		want  []string
	}{
		{"absent", host{CLI: "claude"}, true, nil},
		{"unlistable", host{CLI: "claude", Present: true}, true, nil},
		{"claude install", claude(plugin{}, plugin{}), true, []string{cm + "add " + repo + " --sparse .claude-plugin plugins/claude", ci + "install " + tmag, ci + "install " + peers}},
		{"claude install without peers", claude(plugin{}, plugin{}), false, []string{cm + "add " + repo + " --sparse .claude-plugin plugins/claude", ci + "install " + tmag}},
		{"claude current", claude(cur, cur), true, nil},
		{"claude current without peers", claude(cur, plugin{}), false, nil},
		{"claude peers turned off", claude(cur, cur), false, []string{ci + "uninstall " + peers}},
		{"claude peers turned on", claude(cur, plugin{}), true, []string{cm + "update " + marketplace, ci + "install " + peers}},
		{"claude disabled", claude(off, off), true, []string{ci + "enable " + tmag, ci + "enable " + peers}},
		{"claude outdated", claude(old, old), true, []string{cm + "update " + marketplace, ci + "update " + tmag, ci + "update " + peers}},
		{"claude outdated and disabled", claude(plugin{Installed: true, Version: "0.0.1"}, cur), true, []string{cm + "update " + marketplace, ci + "update " + tmag, ci + "enable " + tmag}},
		{"claude outdated, peers off", claude(old, old), false, []string{cm + "update " + marketplace, ci + "update " + tmag, ci + "uninstall " + peers}},
		{"codex install", codex(plugin{}, plugin{}), true, []string{xm + "add " + repo + " --sparse .agents --sparse plugins/codex", xi + "add " + tmag, xi + "add " + peers}},
		{"codex current", codex(cur, cur), true, nil},
		{"codex outdated", codex(old, old), true, []string{xm + "upgrade " + marketplace, xi + "add " + tmag, xi + "add " + peers}},
		{"codex peers turned on", codex(cur, plugin{}), true, []string{xm + "upgrade " + marketplace, xi + "add " + peers}},
		{"codex peers turned off", codex(cur, cur), false, []string{xi + "remove " + peers}},
		{"codex disabled", codex(off, cur), true, []string{xi + "add " + tmag}},
		// Pi and oh-my-pi install the tmux plugin's own packages by path.
		{"pi install", pi(plugin{}, plugin{}), true, []string{"pi install /p/plugins/pi/tmag", "pi install /p/plugins/pi/tmag-peers"}},
		{"pi current", pi(cur, cur), true, nil},
		{"pi peers turned off", pi(cur, cur), false, []string{"pi remove /p/plugins/pi/tmag-peers"}},
		{"pi from another plugin directory", pi(plugin{Elsewhere: []string{"/old/plugins/pi/tmag"}}, cur), true, []string{"pi remove /old/plugins/pi/tmag", "pi install /p/plugins/pi/tmag"}},
		{"pi disabled in its configuration", pi(off, off), true, nil},
		{"pi without the tmux plugin", host{CLI: "pi", Present: true, Listed: true}, true, nil},
		{"omp install", omp(plugin{}, plugin{}), true, []string{"omp plugin install /p/plugins/omp/tmag", "omp plugin install /p/plugins/omp/tmag-peers"}},
		{"omp current", omp(cur, cur), true, nil},
		{"omp disabled", omp(off, cur), true, []string{"omp plugin enable tmag"}},
		{"omp linked elsewhere", omp(plugin{Elsewhere: []string{"tmag"}}, cur), true, []string{"omp plugin install /p/plugins/omp/tmag"}},
		{"omp peers turned off", omp(cur, cur), false, []string{"omp plugin uninstall tmag-peers"}},
		{"omp peers off, linked elsewhere", omp(cur, plugin{Elsewhere: []string{"tmag-peers"}}), false, []string{"omp plugin uninstall tmag-peers"}},
	} {
		var got []string
		for _, argv := range setupPlan(c.h, c.peers) {
			got = append(got, strings.Join(argv, " "))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: plan\n  %s\nwant\n  %s", c.name, strings.Join(got, "\n  "), strings.Join(c.want, "\n  "))
		}
	}
}
