package hook

import (
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Claude Code names its status file under CLAUDE_CONFIG_DIR when that is an
// absolute directory, else under $HOME/.claude; nothing else names one.
func TestStatusFileOf(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(name string) string { return vals[name] }
	}
	for _, c := range []struct {
		name  string
		agent string
		pid   int
		env   map[string]string
		want  string
	}{
		{"claude under home", proc.Claude, 42, map[string]string{"HOME": "/home/me"}, "/home/me/.claude/sessions/42.json"},
		{"claude under its config dir", proc.Claude, 42, map[string]string{"HOME": "/home/me", claudeConfigVar: "/cfg"}, "/cfg/sessions/42.json"},
		{"a relative config dir names nothing", proc.Claude, 42, map[string]string{"HOME": "/home/me", claudeConfigVar: "cfg"}, ""},
		{"no home", proc.Claude, 42, map[string]string{}, ""},
		{"another agent", proc.Codex, 42, map[string]string{"HOME": "/home/me"}, ""},
		{"no host", proc.Claude, 0, map[string]string{"HOME": "/home/me"}, ""},
		{"too long", proc.Claude, 42, map[string]string{claudeConfigVar: "/" + strings.Repeat("d", adapter.MaxPath)}, ""},
	} {
		if got := statusFileOf(c.agent, c.pid, env(c.env)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
