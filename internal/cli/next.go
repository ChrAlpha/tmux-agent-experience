package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

// nextPrefix starts the line after an error that says what to do next.
const nextPrefix = "next: "

// delivered marks an error that ended a command after it delivered a
// prompt: whatever went wrong, the prompt must not be sent again.
type delivered struct{ err error }

func (e delivered) Error() string { return e.err.Error() }
func (e delivered) Unwrap() error { return e.err }

// undelivered marks an error that proves nothing was delivered, so the same
// command may run again.
type undelivered struct{ err error }

func (e undelivered) Error() string { return e.err.Error() }
func (e undelivered) Unwrap() error { return e.err }

// notShown marks a wait that ran out while its pane still showed no session.
type notShown struct{ err error }

func (e notShown) Error() string { return e.err.Error() }
func (e notShown) Unwrap() error { return e.err }

// next says what to do after err ended cmd on target, for the outcomes a
// person or an agent meets when working with another agent; "" when the
// error says it all, including whenever it cannot tell whether a prompt was
// delivered. It is printed to stderr only, so JSON documents keep their
// contract, and teaching it there keeps it out of the skill: an agent reads
// it only when it meets the outcome.
func next(cmd, target string, err error) string {
	if target == "" {
		target = "ID"
	}
	var started *query.StartedError
	if errors.As(err, &started) {
		return afterStart(started)
	}
	var notTaken *query.NotTakenError
	if errors.As(err, &notTaken) {
		return fmt.Sprintf("nothing was delivered: run the same tmag %s again", cmd)
	}
	if errors.As(err, new(delivered)) {
		if errors.Is(err, query.ErrEnded) {
			return "the prompt arrived, but its session ended: tmag list shows the agents that run"
		}
		return fmt.Sprintf("the prompt arrived and what became of it is unknown: run tmag wait %s --for idle,blocked, then tmag read %s --last; do not send the prompt again", target, target)
	}
	if errors.As(err, new(undelivered)) {
		return fmt.Sprintf("nothing was delivered: run the same tmag %s again", cmd)
	}
	if errors.As(err, new(*query.UnshownError)) {
		return fmt.Sprintf("an agent shows its session from its first hook: tmag wait %s --for idle waits for it, then try once more; tmag list shows what runs there", target)
	}
	var refused *query.RefusedError
	if errors.As(err, &refused) {
		return nextAfterRefusal(cmd, target, refused.Reason)
	}
	switch {
	case errors.Is(err, errUnshown):
		return fmt.Sprintf("tmag wait %s --for idle waits until it shows its session", target)
	case errors.As(err, new(notShownStart)):
		return fmt.Sprintf("its window stays: see what holds it with tmag read %s --lines 40. An agent at a trust dialog or a login waits for the person, who answers it there or with tmag keys; Codex in its shared daemon shows its session only with the thread-id title item", target)
	case errors.Is(err, query.ErrNoWindow):
		return "run tmag start in a tmux pane: the agent's window opens beside it"
	case errors.As(err, new(notShown)):
		return "it still shows no session: tmag list shows what runs there. An agent at a trust dialog waits for the person; Codex shows its session from its first prompt, and in its shared daemon only with the thread-id title item"
	case cmd == "wait" && (errors.Is(err, errTimeout) || errors.Is(err, context.DeadlineExceeded)):
		return "the state did not come in time: run the same tmag wait again"
	case errors.Is(err, query.ErrEnded), errors.Is(err, query.ErrNotFound):
		return "tmag list shows the agents that run"
	case errors.Is(err, query.ErrAmbiguous):
		return "name it by a longer id or by its pane, as tmag list shows them"
	case errors.Is(err, query.ErrNoReply):
		return fmt.Sprintf("see what its pane shows with tmag read %s", target)
	}
	return ""
}

// afterStart says what to do after a start whose agent started and whose
// prompt then failed: never start it again, which starts another.
func afterStart(e *query.StartedError) string {
	id := shortID(e.Session.ID)
	var notTaken *query.NotTakenError
	var refused *query.RefusedError
	switch {
	case errors.Is(e.Err, query.ErrNotGiven) || errors.As(e.Err, &notTaken) || errors.As(e.Err, &refused):
		return fmt.Sprintf("it runs as %s, and has not been given the prompt: give it with tmag send %s -, asking as you did", id, id)
	case errors.Is(e.Err, query.ErrUnlogged):
		return fmt.Sprintf("it runs as %s: tmag list shows it", id)
	}
	return fmt.Sprintf("it runs as %s, and what became of the prompt is unknown: run tmag wait %s --for idle,blocked, then tmag read %s --last; do not send the prompt again", id, id, id)
}

// nextAfterRefusal says what to do after a refusal, which delivers nothing:
// every refusal reason has an answer, which TestEveryRefusalSaysWhatNext
// checks.
func nextAfterRefusal(cmd, target, reason string) string {
	switch reason {
	case query.RefuseBusy:
		if cmd == "interrupt" {
			return fmt.Sprintf("it was just interrupted and has not reported since: tmag wait %s --for idle shows when it stops", target)
		}
		if cmd == "send" || cmd == "reply" {
			return fmt.Sprintf("try once more in a few seconds, or wait until it is idle with tmag wait %s --for idle", target)
		}
		return fmt.Sprintf("wait until it is idle with tmag wait %s --for idle, then try once more", target)
	case query.RefuseSubagent:
		return fmt.Sprintf("wait until it is idle with tmag wait %s --for idle, then try once more", target)
	case query.RefuseChanged:
		if cmd == "keys" {
			return fmt.Sprintf("see what it shows now with tmag read %s --lines 40 --json, and type again for that screen", target)
		}
		return "run tmag list and decide again"
	case query.RefuseIdle:
		return "nothing to do: it runs no turn"
	case query.RefuseUnbound:
		return "nothing tmag can prove reaches it: tmag list shows what it shows, and the person can act in the agent"
	case query.RefuseBackground:
		return "only the person can reach it: its agent must hold its terminal's foreground"
	case query.RefuseUnsupported:
		return "only the person can do this, in the agent itself"
	case query.RefuseSelf:
		return "this is your own session: use another agent's id from tmag list"
	case query.RefuseAgent:
		if cmd == "keys" {
			return "only the person types into a pane: talk to the agent with tmag send"
		}
		return "only the person can decide this"
	case query.RefuseUnasked:
		return fmt.Sprintf("answer an ask of send --wait in your final message; to tell %s something it did not ask for, use tmag send %s -", target, target)
	case query.RefuseCycle:
		return fmt.Sprintf("ask it with tmag send %s --reply - instead, which does not hold your turn, and finish your turn", target)
	case query.RefuseDepth:
		return "do the work yourself, or tell the person what needs another agent"
	case query.RefuseOwing:
		return fmt.Sprintf("ask another agent, or wait for %s's answers with tmag wait %s --for idle", target, target)
	}
	return ""
}
