package hook

import (
	"context"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// A turn is seen only where a watched pane shows it on a server that had the
// person's input, unless that server turned seen turns off.
func TestSeenAtTheTerminal(t *testing.T) {
	ctx := context.Background()
	watched := tmux.Pane{Server: "/a", ID: "%1", Watched: true}
	unwatched := tmux.Pane{Server: "/a", ID: "%1"}
	other := tmux.Pane{Server: "/b", ID: "%2", Watched: true}
	on := &optionCache{values: map[string]serverOptions{"/a": {seen: "on"}, "/b": {seen: "off"}}}
	for _, c := range []struct {
		name    string
		shown   []tmux.Pane
		present map[string]bool
		opts    *optionCache
		want    bool
	}{
		{"watched, present and on", []tmux.Pane{watched}, map[string]bool{"/a": true}, on, true},
		{"nobody watching", []tmux.Pane{unwatched}, map[string]bool{"/a": true}, on, false},
		{"nobody at the terminal", []tmux.Pane{watched}, map[string]bool{"/a": false}, on, false},
		{"no pane shows it", nil, map[string]bool{"/a": true}, on, false},
		{"turned off there", []tmux.Pane{other}, map[string]bool{"/b": true}, on, false},
		{"another watched server is on", []tmux.Pane{other, watched}, map[string]bool{"/a": true, "/b": true}, on, true},
	} {
		if got := seenAt(c.shown, c.present, c.opts, ctx); got != c.want {
			t.Errorf("%s: seenAt = %v, want %v", c.name, got, c.want)
		}
	}
}
