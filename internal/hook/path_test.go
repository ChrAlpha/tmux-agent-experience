package hook

import (
	"context"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// A hook takes the full proof path, listing panes and reading the process
// table, only when its host is a client under tmux whose pane the record
// does not list. A service, or a client outside tmux, runs in no pane: its
// tool hooks start no tmux process and read no process table.
func TestOnlyAnUnlistedClientUnderTmuxTakesTheFullPath(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	for _, c := range []struct {
		name  string
		agent string
		chain []proc.Process
		role  proc.Role
		full  bool
	}{
		{"a Codex daemon", proc.Codex, []proc.Process{
			{PID: 1, Comm: "init"},
			{PID: 10, PPID: 1, Comm: "codex", Args: "codex app-server", Argv: []string{"codex", "app-server"}},
			{PID: 11, PPID: 10, Comm: "sh"},
		}, proc.Service, false},
		{"a client outside tmux", proc.Claude, []proc.Process{
			{PID: 1, Comm: "init"},
			{PID: 10, PPID: 1, Comm: "sshd"},
			{PID: 11, PPID: 10, Comm: "claude", Args: "claude", Start: "s11"},
			{PID: 12, PPID: 11, Comm: "sh"},
		}, proc.Terminal, false},
		{"a client in a pane", proc.Claude, []proc.Process{
			{PID: 1, Comm: "init"},
			{PID: 10, PPID: 1, Comm: "tmux: server"},
			{PID: 11, PPID: 10, Comm: "zsh"},
			{PID: 12, PPID: 11, Comm: "claude", Args: "claude", Start: "s12"},
			{PID: 13, PPID: 12, Comm: "sh"},
		}, proc.Terminal, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := eventlog.Open(t.TempDir())
			listed := false
			sources := Sources{
				Lineage: func(context.Context, int) (*proc.Table, error) { return proc.NewTable(c.chain), nil },
				Panes: func(context.Context) ([]tmux.Pane, error) {
					listed = true
					return nil, nil
				},
				Processes: func(context.Context) (*proc.Table, error) { return proc.NewTable(c.chain), nil },
			}
			hook := c.chain[len(c.chain)-1].PID
			for _, payload := range []string{
				`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt_id":"turn"}`,
				`{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"call"}`,
			} {
				in := Input{Agent: c.agent, Payload: strings.NewReader(payload), Log: log, PID: hook, Sources: sources}
				if err := Run(context.Background(), in); err != nil {
					t.Fatal(err)
				}
			}
			if listed != c.full {
				t.Fatalf("listed panes: %v, want %v", listed, c.full)
			}
			rec, ok, err := (session.Store{Dir: log.Dir}).Get(context.Background(), c.agent, "s1")
			if err != nil || !ok || rec.State() != "working" || rec.Host.Role != c.role {
				t.Fatalf("recorded %+v, %v, %v; want working, hosted as %s", rec, ok, err, c.role)
			}
		})
	}
}
