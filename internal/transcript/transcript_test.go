package transcript

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Lines shaped as Claude Code 2.1.317 writes them: one line per content
// block, every user entry naming its prompt.
const (
	claudePrompt1 = `{"type":"user","promptId":"p1","isSidechain":false,"message":{"role":"user","content":"first"}}`
	claudeReply1  = `{"type":"assistant","isSidechain":false,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"one"}]}}`
	claudePrompt2 = `{"type":"user","promptId":"p2","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"second"}]}}`
	claudeThink   = `{"type":"assistant","isSidechain":false,"message":{"id":"m2","role":"assistant","content":[{"type":"thinking","thinking":"hm"}]}}`
	claudeLook    = `{"type":"assistant","isSidechain":false,"message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"Let me look."}]}}`
	claudeTool    = `{"type":"assistant","isSidechain":false,"message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"u1","name":"Bash","input":{}}]}}`
	claudeResult  = `{"type":"user","promptId":"p2","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"u1","content":"ok"}]}}`
	claudeFinalA  = `{"type":"assistant","isSidechain":false,"message":{"id":"m3","role":"assistant","content":[{"type":"text","text":"Found it."}]}}`
	claudeFinalB  = `{"type":"assistant","isSidechain":false,"message":{"id":"m3","role":"assistant","content":[{"type":"text","text":"It is fine."}]}}`
	claudeSide    = `{"type":"assistant","isSidechain":true,"message":{"id":"s1","role":"assistant","content":[{"type":"text","text":"subagent"}]}}`
	claudeMeta    = `{"type":"user","promptId":"p9","isMeta":true,"message":{"role":"user","content":"caveat"}}`
	claudeOther   = `{"type":"system","subtype":"turn_duration"}`
)

func TestClaudeLastReply(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		lines []string
		want  Reply
	}{
		{"a plain turn", []string{claudePrompt1, claudeReply1, claudeOther}, Reply{"p1", "one"}},
		{"the final message after tools, whole",
			[]string{claudePrompt1, claudeReply1, claudePrompt2, claudeThink, claudeLook, claudeTool, claudeResult, claudeFinalA, claudeFinalB, claudeOther},
			Reply{"p2", "Found it.\n\nIt is fine."}},
		{"not a subagent's or a meta entry",
			[]string{claudePrompt1, claudeReply1, claudeSide, claudeMeta, claudeOther}, Reply{"p1", "one"}},
		{"a turn with no text yet", []string{claudePrompt1, claudeReply1, claudePrompt2, claudeTool}, Reply{"p2", ""}},
	} {
		got, err := Last(ctx, write(t, c.lines...), ClaudeJSONL)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	if _, err := Last(ctx, write(t, claudeOther), ClaudeJSONL); !errors.Is(err, ErrNoTurn) {
		t.Errorf("no turn: %v", err)
	}
}

// Lines shaped as Codex 0.166 writes its rollout.
const (
	codexMeta     = `{"timestamp":"t","type":"session_meta","payload":{"id":"th"}}`
	codexStarted1 = `{"timestamp":"t","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`
	codexMessage  = `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"draft"}]}}`
	codexDone1    = `{"timestamp":"t","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"all done"}}`
	codexStarted2 = `{"timestamp":"t","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}`
	codexDone2    = `{"timestamp":"t","type":"event_msg","payload":{"type":"task_complete","turn_id":"t2","last_agent_message":null}}`
	codexQuoted   = `{"timestamp":"t","type":"response_item","payload":{"type":"function_call_output","output":"\"task_complete\""}}`
)

func TestCodexLastReply(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		lines []string
		want  Reply
	}{
		{"the turn's last message", []string{codexMeta, codexStarted1, codexMessage, codexDone1}, Reply{"t1", "all done"}},
		{"a turn without one", []string{codexMeta, codexStarted1, codexDone1, codexStarted2, codexDone2}, Reply{"t2", ""}},
		{"the last finished turn", []string{codexMeta, codexStarted1, codexDone1, codexStarted2, codexMessage, codexQuoted}, Reply{"t1", "all done"}},
	} {
		got, err := Last(ctx, write(t, c.lines...), CodexRollout)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	if _, err := Last(ctx, write(t, codexMeta, codexStarted1), CodexRollout); !errors.Is(err, ErrNoTurn) {
		t.Errorf("no turn: %v", err)
	}
}

// Lines cross chunk boundaries and outgrow a chunk; a line still being
// written comes as it is.
func TestBackwardAcrossChunks(t *testing.T) {
	long := strings.Repeat("x", 3*chunk)
	lines := []string{"a", long, "", "b", strings.Repeat("y", chunk-1), "c"}
	path := filepath.Join(t.TempDir(), "t")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []string
	if err := backward(context.Background(), f, func(b []byte) bool { got = append(got, string(b)); return false }); err != nil {
		t.Fatal(err)
	}
	want := []string{"{partial", "c", lines[4], "b", long, "a"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
}
