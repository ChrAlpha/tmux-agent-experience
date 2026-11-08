package cli

import (
	"context"

	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
)

// hookContext preserves Main's entry budget, while in-process callers supply
// their own parent. Reusable command handlers never install a process cutoff.
func (a *app) hookContext() (context.Context, context.CancelFunc) {
	if a.setup != nil {
		return context.WithCancel(a.setup)
	}
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, hook.Deadline)
}
