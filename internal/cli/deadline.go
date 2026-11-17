package cli

import (
	"context"
	"time"

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

// startedAt is the process's start, or now in an in-process caller that did
// not record one.
func (a *app) startedAt() time.Time {
	if a.started.IsZero() {
		return time.Now()
	}
	return a.started
}

// reportContext is the budget a hook report may use after its setup budget
// has run out: less than the 250 ms Main reserves before the hard cutoff. A
// signal cancels the invocation, not the report of the failure it caused.
func (a *app) reportContext() (context.Context, context.CancelFunc) {
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), 200*time.Millisecond)
}

// recordHookError writes a hook failure from the reserve between the setup
// deadline and the hard cutoff. A hook that runs out of its setup budget has
// an expired setup context, so its report gets a new bounded budget instead
// of being lost with it.
func (a *app) recordHookError(agent string, err error) {
	ctx, cancel := a.reportContext()
	defer cancel()
	hook.RecordError(ctx, a.log.Dir, agent, err)
}

// recordHookSlow writes a successful hook that took longer than
// hook.SlowAfter from start; nothing is written when the hook was quick.
func (a *app) recordHookSlow(agent string, start time.Time) {
	if start.IsZero() || time.Since(start) <= hook.SlowAfter {
		return
	}
	ctx, cancel := a.reportContext()
	defer cancel()
	hook.RecordSlow(ctx, a.log.Dir, agent)
}
