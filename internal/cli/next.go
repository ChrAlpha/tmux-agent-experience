package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

// nextPrefix starts the line after an error that says what to do next.
const nextPrefix = "next: "

// next says what to do after err ended cmd on target, for the outcomes a
// person or an agent meets when working with another agent; "" when the
// error says it all. It is printed to stderr only, so JSON documents keep
// their contract, and teaching it there keeps it out of the skill: an agent
// reads it only when it meets the outcome.
func next(cmd, target string, err error) string {
	if target == "" {
		target = "ID"
	}
	var refused *query.RefusedError
	if errors.As(err, &refused) {
		return nextAfterRefusal(cmd, target, refused.Reason)
	}
	switch {
	case errors.Is(err, errTimeout), errors.Is(err, context.DeadlineExceeded):
		switch cmd {
		case "send":
			return fmt.Sprintf("the prompt arrived and %s is still busy: run tmag wait %s, then tmag read %s --last; do not send it again", target, target, target)
		case "wait":
			return fmt.Sprintf("it is still busy: run tmag wait %s again", target)
		case "reply":
			return "nothing was delivered: run the same tmag reply again"
		}
	case errors.Is(err, eventlog.ErrHistoryUnavailable), errors.Is(err, eventlog.ErrIntegrityLoss):
		if cmd == "send" {
			return "the prompt arrived: see its state with tmag list; do not send it again"
		}
		return "see its state with tmag list"
	case errors.Is(err, query.ErrEnded), errors.Is(err, query.ErrNotFound):
		return "tmag list shows the agents that run"
	case errors.Is(err, query.ErrAmbiguous):
		return "name it by a longer id or by its pane, as tmag list shows them"
	case errors.Is(err, query.ErrNoReply):
		return fmt.Sprintf("see what its pane shows with tmag read %s", target)
	}
	return ""
}

// nextAfterRefusal says what to do after a refusal: every refusal reason
// has an answer, which TestEveryRefusalSaysWhatNext checks.
func nextAfterRefusal(cmd, target, reason string) string {
	switch reason {
	case query.RefuseBusy:
		if cmd == "interrupt" {
			return fmt.Sprintf("it was just interrupted and has not reported since: tmag wait %s shows when it stops", target)
		}
		return fmt.Sprintf("wait until it is idle with tmag wait %s --for idle, then try once more", target)
	case query.RefuseSubagent:
		return fmt.Sprintf("wait until it is idle with tmag wait %s --for idle, then try once more", target)
	case query.RefuseChanged:
		return "run tmag list and decide again"
	case query.RefuseIdle:
		return "nothing to do: it runs no turn"
	case query.RefuseUnbound:
		return "only the person can reach it: tmag needs a tmux pane proven to show it"
	case query.RefuseBackground:
		return "only the person can reach it: its agent must hold its terminal's foreground"
	case query.RefuseUnsupported:
		return "only the person can do this, in the agent itself"
	case query.RefuseSelf:
		return "this is your own session: use another agent's id from tmag list"
	case query.RefuseAgent:
		return "only the person can decide this"
	}
	return ""
}
