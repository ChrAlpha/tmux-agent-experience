package e2e

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/transcript"
)

// read --turns gives a session's turns from the agent's own transcript in
// any state, oldest first: each prompt and the reply read --last gives.
func TestReadTurns(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi"} {
		t.Run(agent, func(t *testing.T) {
			h := start(t)
			pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "300"}, filepath.Join(bin, agent))
			h.waitPane(pane, "idle", "process")
			turns := func(n string) []transcript.Turn {
				t.Helper()
				r := h.tmag("", "read", h.target(pane), "--turns", n, "--json")
				var doc struct {
					Turns []transcript.Turn `json:"turns"`
				}
				if r.code != 0 || json.Unmarshal([]byte(r.out), &doc) != nil {
					t.Fatalf("read --turns %s: %d %s", n, r.code, r.out)
				}
				return doc.Turns
			}
			if got := turns("5"); len(got) != 0 {
				t.Fatalf("turns before the first prompt: %+v", got)
			}
			for _, prompt := range []string{"hello", "line one\nagain"} {
				if r := h.tmag(prompt, "send", h.target(pane), "--wait", "-"); r.code != 0 {
					t.Fatalf("send %q: %d %s", prompt, r.code, r.out)
				}
			}
			s, _ := h.sessionOf(sidA)
			want := []transcript.Turn{
				{ID: "t1", Prompts: []transcript.Prompt{{Text: "hello"}}, Reply: "answered: hello"},
				{ID: s.Turn, Prompts: []transcript.Prompt{{Text: "line one\nagain"}}, Reply: "answered: again"},
			}
			if got := turns("5"); !reflect.DeepEqual(got, want) {
				t.Fatalf("turns:\n got %+v\nwant %+v", got, want)
			}
			if r := h.tmag("", "read", sidA[:8], "--turns", "1"); r.code != 0 || r.out != "> line one\n> again\nanswered: again\n" {
				t.Fatalf("read --turns 1: %d %q", r.code, r.out)
			}

			if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 {
				t.Fatalf("send: %d %s", r.code, r.out)
			}
			h.waitSession(sidA, "working")
			if got := turns("1"); len(got) != 1 || !reflect.DeepEqual(got[0].Prompts, []transcript.Prompt{{Text: "long work"}}) || got[0].Reply != "" {
				t.Fatalf("a running turn: %+v", got)
			}
			if r := h.tmag("", "read", sidA[:8], "--turns", "21"); r.code != 2 || !strings.Contains(r.out, "1 to 20") {
				t.Fatalf("read --turns 21: %d %s", r.code, r.out)
			}
		})
	}
}
