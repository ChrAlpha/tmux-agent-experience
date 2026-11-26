package tmux

import (
	"slices"
	"testing"
)

// tmux ends a command at an argument that ends in ";": an argument that only
// ends in it keeps it, and ";" alone still separates commands.
func TestLiteralArgs(t *testing.T) {
	got := literalArgs([]string{"new-window", "-c", "/tmp/d;", ";", "display", "a;b", `x\;`, ";;"})
	want := []string{"new-window", "-c", `/tmp/d\;`, ";", "display", "a;b", `x\\;`, `;\;`}
	if !slices.Equal(got, want) {
		t.Fatalf("literalArgs = %q, want %q", got, want)
	}
}
