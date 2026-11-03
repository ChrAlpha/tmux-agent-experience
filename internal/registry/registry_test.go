package registry

import (
	"slices"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
)

func TestPromptsRoundTrip(t *testing.T) {
	in := []adapter.Prompt{{Tool: "Bash"}, {Agent: "a1", Tool: "mcp__srv__do-it"}, {Agent: "a2"}, {}}
	if got := decodePrompts(encodePrompts(in)); !slices.Equal(got, in) {
		t.Fatalf("round trip = %+v", got)
	}
	if got := decodePrompts(""); got != nil {
		t.Fatalf("empty = %+v", got)
	}
}
