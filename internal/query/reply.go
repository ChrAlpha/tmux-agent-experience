package query

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/inbox"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/transcript"
)

// Reply hands a reply to the asker through its own message channel, whatever
// its state, with the first line that says who answered. A pane-only asker
// gets it through the pane while idle; while it is busy the refusal is busy,
// so the caller may try again. Only an agent whose session tmag proves may
// reply, and only to an ask of the asker's for a reply, which the reply
// settles (see claimReply).
func Reply(ctx context.Context, log *eventlog.Log, target, text string) (Delivery, error) {
	return deliver(ctx, log, target, prompt{text: text, ask: Answering}, "reply", "reply")
}

// ErrNoReply reports a session whose last turn left no reply to read.
var ErrNoReply = errors.New("no reply")

// transcriptLag is how long an agent may take to write a turn's end to its
// transcript after the hook that reported it: Claude Code may still write
// the reply after its Stop hook ran.
const transcriptLag = 3 * time.Second

// LastReply returns an idle session's last turn with the reply that ended
// it, as the agent's transcript holds it: the turn is the one its hooks
// reported, never an earlier one the transcript ends with while the agent
// still writes. tmag reads the transcript for this and keeps nothing of it.
func LastReply(ctx context.Context, store session.Store, target string) (Session, transcript.Turn, error) {
	v, _, err := Load(ctx, store)
	if err != nil {
		return Session{}, transcript.Turn{}, err
	}
	s, _, err := controlled(ctx, v, target)
	if err != nil {
		return Session{}, transcript.Turn{}, err
	}
	switch {
	case s.State != adapter.Idle:
		return s, transcript.Turn{}, &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s; its turn has no final reply yet", s.ID, describe(s.State, s.Kind))}
	case s.Transcript == nil && !proc.Declarable(s.Agent):
		return s, transcript.Turn{}, fmt.Errorf("%w: the hooks of %s did not say where its transcript is", ErrNoReply, s.ID)
	case s.Transcript == nil && turnsGate(s) != nil:
		return s, transcript.Turn{}, turnsGate(s)
	case s.Turn == "":
		return s, transcript.Turn{}, fmt.Errorf("%w: %s has run no turn", ErrNoReply, s.ID)
	}
	deadline := time.Now().Add(transcriptLag)
	for {
		t, err := lastTurn(ctx, v, s)
		switch {
		case err == nil && t.ID == s.Turn && t.Reply != "":
			return s, t, nil
		case err != nil && !errors.Is(err, transcript.ErrNoTurn) && !errors.Is(err, fs.ErrNotExist):
			return s, transcript.Turn{}, err
		case !time.Now().Before(deadline) && err == nil && t.ID == s.Turn:
			return s, t, fmt.Errorf("%w: turn %s of %s ended without one", ErrNoReply, s.Turn, s.ID)
		case !time.Now().Before(deadline):
			return s, transcript.Turn{}, fmt.Errorf("%w: the transcript of %s does not hold the end of its turn %s", ErrNoReply, s.ID, s.Turn)
		}
		select {
		case <-ctx.Done():
			return s, transcript.Turn{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// lastTurn is the last turn a session's agent holds, its reply whole: as its
// transcript holds it, or as a declared agent's inbox gives it.
func lastTurn(ctx context.Context, v View, s Session) (transcript.Turn, error) {
	if s.Transcript != nil {
		return transcript.Last(ctx, s.Transcript.Path, s.Transcript.Format)
	}
	turns, err := inboxTurns(ctx, v, s, 1)
	switch {
	case err != nil:
		return transcript.Turn{}, err
	case len(turns) == 0:
		return transcript.Turn{}, transcript.ErrNoTurn
	}
	return turns[0], nil
}

// inboxTurns asks a declared agent for its session's last n turns through
// the inbox its hooks named, when the driver gives it one: no transcript
// tmag reads holds them.
func inboxTurns(ctx context.Context, v View, s Session, n int) ([]transcript.Turn, error) {
	if err := turnsGate(s); err != nil {
		return nil, err
	}
	rec, _ := v.record(s.Agent, s.ID)
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()
	c, err := inboxAt(ctx, s, rec)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	turns, err := c.Turns(ctx, s.ID, n)
	var refused *inbox.Refusal
	if errors.As(err, &refused) {
		reason := RefuseChanged
		if refused.Reason == RefuseUnsupported {
			reason = refused.Reason
		}
		return nil, &RefusedError{reason, refused.Error()}
	}
	return turns, err
}

// turnsGate refuses a read of a declared session's turns that no channel
// carries: one that serves a program.
func turnsGate(s Session) error {
	if driver.TurnsChannel(s.Agent, s.Runtime) != driver.Inbox {
		return unsupported(s, "a read of its turns")
	}
	return nil
}

// Turns returns the last n turns of a session, in any state, as the agent's
// transcript holds them, or a declared agent's inbox gives them: the last
// one may be running. A session whose transcript is not written yet has
// none. tmag reads the turns for this and keeps nothing of them.
func Turns(ctx context.Context, store session.Store, target string, n int) (Session, []transcript.Turn, error) {
	v, _, err := Load(ctx, store)
	if err != nil {
		return Session{}, nil, err
	}
	s, _, err := controlled(ctx, v, target)
	if err != nil {
		return Session{}, nil, err
	}
	if s.Transcript == nil && proc.Declarable(s.Agent) {
		if n < 1 || n > transcript.MaxTurns {
			return s, nil, fmt.Errorf("read 1 to %d turns", transcript.MaxTurns)
		}
		turns, err := inboxTurns(ctx, v, s, n)
		if err != nil {
			return s, nil, err
		}
		for i := range turns {
			turns[i] = transcript.Bound(turns[i])
		}
		if turns == nil {
			turns = []transcript.Turn{}
		}
		return s, turns, nil
	}
	if s.Transcript == nil {
		return s, nil, fmt.Errorf("%w: the hooks of %s did not say where its transcript is", ErrNoReply, s.ID)
	}
	turns, err := transcript.Turns(ctx, s.Transcript.Path, s.Transcript.Format, n)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, []transcript.Turn{}, nil
	case err != nil:
		return s, nil, err
	case turns == nil:
		turns = []transcript.Turn{}
	}
	return s, turns, nil
}
