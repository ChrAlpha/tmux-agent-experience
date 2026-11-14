package transcript

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Claude Code 2.1.319: a message to a working session is a queued_command
// attachment, to an idle one a meta entry of a peer; both carry the text in
// the origin's body, and tmag names itself for the person. A background
// task's notice is a prompt of its own origin. Interruption notes and
// compaction summaries are user entries of no origin.
const (
	claudeQueued   = `{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","prompt":"<cross-session-message from-name=\"tmag\">\nalso the year\n</cross-session-message>","commandMode":"prompt","origin":{"kind":"peer","from":"unknown","name":"tmag","body":"also the year"}}}`
	claudeTyped    = `{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","prompt":"and the month","commandMode":"prompt","origin":{"kind":"human"},"humanTurn":true}}`
	claudeTmagPeer = `{"type":"user","promptId":"p11","isMeta":true,"isSidechain":false,"origin":{"kind":"peer","from":"unknown","name":"claude 1787a44e via tmag","body":"[tmag] From claude 1787a44e, another agent. No answer needed.\nping"},"message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from-name=\"claude 1787a44e via tmag\">\n…"}}`
	claudePong     = `{"type":"assistant","isSidechain":false,"message":{"id":"m5","role":"assistant","content":[{"type":"text","text":"pong"}]}}`
	claudeTask     = `{"type":"user","promptId":"p12","isSidechain":false,"origin":{"kind":"task-notification","producer":"session-task"},"promptSource":"system","message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Background command \"Sleep\" completed (exit code 0)</summary>\n</task-notification>"}}`
	claudeNoted    = `{"type":"assistant","isSidechain":false,"message":{"id":"m6","role":"assistant","content":[{"type":"text","text":"Noted."}]}}`
	claudeStop     = `{"type":"user","promptId":"p2","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
	claudeBash     = `{"type":"user","promptId":"p3","isSidechain":false,"message":{"role":"user","content":"<bash-input>ls</bash-input>"}}`
	claudeBashOut  = `{"type":"user","promptId":"p3","isSidechain":false,"message":{"role":"user","content":"<bash-stdout>go.mod</bash-stdout><bash-stderr></bash-stderr>"}}`
	claudeLocal    = `{"type":"user","promptId":"p4","isSidechain":false,"message":{"role":"user","content":"<command-name>/model</command-name>"}}`
	claudeLocalOut = `{"type":"user","promptId":"p4","isSidechain":false,"message":{"role":"user","content":"<local-command-stdout>Set model</local-command-stdout>"}}`
	claudeCompact  = `{"type":"user","promptId":"p2","isSidechain":false,"isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}`
)

// Codex 0.167 logs each prompt as a UserMessage item of its turn, a steer
// too.
const (
	codexAsk1  = `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"first","text_elements":[]}]}}}`
	codexAsk2  = `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"UserMessage","id":"u2","content":[{"type":"text","text":"second","text_elements":[]}]}}}`
	codexSteer = `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"UserMessage","id":"u3","content":[{"type":"text","text":"and quickly","text_elements":[]}]}}}`
	codexRan   = `{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"CommandExecution","id":"c1","command":["bash","-lc","ls"],"aggregated_output":"UserMessage task_complete","status":"completed","exit_code":0}}}`
)

func turnsOf(t *testing.T, format string, n int, lines ...string) []Turn {
	t.Helper()
	got, err := Turns(context.Background(), write(t, lines...), format, n)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func same(t *testing.T, name string, got, want []Turn) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
	}
}

func TestClaudeTurns(t *testing.T) {
	two := []string{claudePrompt1, claudeReply1, claudeOther, claudePrompt2, claudeThink, claudeLook, claudeTool,
		claudeQueued, claudeTyped, claudeResult, claudeFinalA, claudeFinalB, claudeOther}
	want := []Turn{
		{ID: "p1", Prompts: []Prompt{{Text: "first"}}, Reply: "one"},
		{ID: "p2", Prompts: []Prompt{{Text: "second"}, {Text: "also the year"}, {Text: "and the month"}}, Reply: "Found it.\n\nIt is fine."},
	}
	same(t, "both turns, with what came while the second ran", turnsOf(t, ClaudeJSONL, 5, two...), want)
	same(t, "the last turn", turnsOf(t, ClaudeJSONL, 1, two...), want[1:])

	same(t, "notes and summaries start no turn",
		turnsOf(t, ClaudeJSONL, 5, claudePrompt1, claudeReply1, claudePrompt2, claudeTool, claudeStop, claudeCompact, claudeFinalA),
		[]Turn{{ID: "p1", Prompts: []Prompt{{Text: "first"}}, Reply: "one"}, {ID: "p2", Prompts: []Prompt{{Text: "second"}}, Reply: "Found it."}})

	same(t, "peers' messages, a task's notice, a subagent and other meta entries",
		turnsOf(t, ClaudeJSONL, 5, claudePeer, claudePeerBack, claudeTmagPeer, claudeSide, claudePong, claudeMeta, claudeTask, claudeNoted),
		[]Turn{
			{ID: "p10", Prompts: []Prompt{{From: "peer"}}, Reply: "peer answer"},
			{ID: "p11", Prompts: []Prompt{{Text: "[tmag] From claude 1787a44e, another agent. No answer needed.\nping", From: "peer"}}, Reply: "pong"},
			{ID: "p12", Prompts: []Prompt{{Text: `Background command "Sleep" completed (exit code 0)`, From: "task"}}, Reply: "Noted."},
		})

	same(t, "a turn the agent ran for a shell command, and a local command that is none",
		turnsOf(t, ClaudeJSONL, 5, claudePrompt1, claudeReply1, claudeBash, claudeBashOut, claudeThink, claudeTool, claudeResult, claudeLocal, claudeLocalOut),
		[]Turn{{ID: "p1", Prompts: []Prompt{{Text: "first"}}, Reply: "one"}, {ID: "p3", Prompts: []Prompt{}, Reply: ""}})
	same(t, "a running turn with no text yet", turnsOf(t, ClaudeJSONL, 5, claudePrompt1, claudeReply1, claudePrompt2, claudeTool),
		[]Turn{{ID: "p1", Prompts: []Prompt{{Text: "first"}}, Reply: "one"}, {ID: "p2", Prompts: []Prompt{{Text: "second"}}, Reply: ""}})
	same(t, "no turn", turnsOf(t, ClaudeJSONL, 5, claudeOther), nil)
}

func TestCodexTurns(t *testing.T) {
	lines := []string{codexMeta, codexStarted1, codexAsk1, codexMessage, codexDone1,
		codexStarted2, codexAsk2, codexRan, codexSteer, codexQuoted, codexDone2}
	want := []Turn{
		{ID: "t1", Prompts: []Prompt{{Text: "first"}}, Reply: "all done"},
		{ID: "t2", Prompts: []Prompt{{Text: "second"}, {Text: "and quickly"}}, Reply: ""},
	}
	same(t, "both turns, a steer in the second", turnsOf(t, CodexRollout, 5, lines...), want)
	same(t, "the last turn", turnsOf(t, CodexRollout, 1, lines...), want[1:])
	same(t, "a running turn, and one no prompt started",
		turnsOf(t, CodexRollout, 5, codexMeta, codexStarted1, codexDone1, codexStarted2, codexAsk2),
		[]Turn{{ID: "t1", Prompts: []Prompt{}, Reply: "all done"}, {ID: "t2", Prompts: []Prompt{{Text: "second"}}}})
}

func TestPiTurns(t *testing.T) {
	lines := []string{piHeader, piModel, piSystem, piPrompt1, piReply1, piPrompt2, piCall, piResult, piFinal, piCustom}
	want := []Turn{
		{ID: "73544b94", Prompts: []Prompt{{Text: "first"}}, Reply: "one"},
		{ID: "8525eceb", Prompts: []Prompt{{Text: "second"}}, Reply: "Found it.\n\nIt is fine."},
	}
	same(t, "both turns", turnsOf(t, PiJSONL, 5, lines...), want)
	same(t, "the last turn", turnsOf(t, PiJSONL, 1, lines...), want[1:])
	same(t, "an aborted turn", turnsOf(t, PiJSONL, 1, piHeader, piPrompt1, piReply1, piPrompt2, piAborted),
		[]Turn{{ID: "8525eceb", Prompts: []Prompt{{Text: "second"}}}})
}

// Whatever the transcript holds, a turn read stays small: its texts are cut
// at a character's boundary, and of many prompts the first and the last ones
// are kept. Last gives the reply whole, as read --last prints it.
func TestTurnsBounds(t *testing.T) {
	long := strings.Repeat("é", maxText)
	lines := []string{`{"type":"user","promptId":"p1","isSidechain":false,"origin":{"kind":"human"},"message":{"role":"user","content":"` + long + `"}}`}
	for i := range maxPrompts + 5 {
		lines = append(lines, `{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","prompt":"q`+string(rune('a'+i))+`","origin":{"kind":"human"}}}`)
	}
	lines = append(lines, `{"type":"assistant","isSidechain":false,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"`+long+`"}]}}`)
	got := turnsOf(t, ClaudeJSONL, 1, lines...)
	if len(got) != 1 {
		t.Fatalf("got %d turns", len(got))
	}
	p := got[0].Prompts
	if len(p) != maxPrompts || p[1].Text != "q"+string(rune('a'+6)) || p[maxPrompts-1].Text != "q"+string(rune('a'+maxPrompts+4)) {
		t.Errorf("prompts kept: %d, %q … %q", len(p), p[1].Text, p[len(p)-1].Text)
	}
	for _, s := range []string{p[0].Text, got[0].Reply} {
		if len(s) > maxText || !utf8.ValidString(s) || !strings.HasSuffix(s, "…") {
			t.Errorf("a text of %d bytes, valid %v", len(s), utf8.ValidString(s))
		}
	}
	if whole, err := Last(context.Background(), write(t, lines...), ClaudeJSONL); err != nil || whole.Reply != long {
		t.Errorf("Last cut the reply to %d bytes, %v", len(whole.Reply), err)
	}
	for _, n := range []int{0, MaxTurns + 1} {
		if _, err := Turns(context.Background(), write(t, claudeOther), ClaudeJSONL, n); err == nil {
			t.Errorf("%d turns read", n)
		}
	}
}
