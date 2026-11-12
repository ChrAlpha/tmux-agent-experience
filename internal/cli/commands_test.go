package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// A blocked session's open requests are listed by inspect without --json,
// under the ids request and answer take: the skill sends the person there
// for a blocked peer.
func TestInspectListsOpenRequests(t *testing.T) {
	// Inspect loads the world: keep its tmux discovery off the developer's
	// server.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	log := eventlog.Open(t.TempDir())
	bash := adapter.Prompt{Tool: "Bash"}
	write := adapter.Prompt{Agent: "a7f3e1c29b04", Tool: "Write"}
	elicit := adapter.Prompt{Tool: "@elicitation"}
	rec := session.Record{
		Agent: "claude", ID: "1787a44e-5b0c-4c39-9a6e-0d6c3c2f7b10", Main: adapter.Working,
		Pending:  []adapter.Prompt{bash, write, elicit},
		Requests: []session.Request{{ID: "3fdce4c70dfa", Prompt: bash}, {ID: "9b41e07a2c55", Prompt: write}, {ID: "c0d2a8f61e93", Prompt: elicit}},
	}
	if err := (session.Store{Dir: log.Dir}).Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := app{ctx: context.Background(), log: log, stdout: &stdout, stderr: &stderr}
	if code := a.run([]string{"inspect", "1787"}); code != ExitOK {
		t.Fatalf("inspect: %d %s", code, stderr.String())
	}
	want := "request: 3fdce4c70dfa (permission, Bash)\n" +
		"request: 9b41e07a2c55 (permission, Write, subagent a7f3e1c29b04)\n" +
		"request: c0d2a8f61e93 (question)\n"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("inspect printed\n%s\nwithout\n%s", stdout.String(), want)
	}
}
