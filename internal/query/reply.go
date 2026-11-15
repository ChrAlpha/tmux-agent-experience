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
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/transcript"
)

// Reply hands a reply to the asker through its own message channel, whatever
// its state, with the first line that says who answered. A pane-only asker
// gets it through the pane while idle; while it is busy the refusal is busy,
// so the caller may try again. Only an agent whose session tmag proves may
// reply.
func Reply(ctx context.Context, log *eventlog.Log, target, text string) (Delivery, error) {
	return deliver(ctx, log, target, text, Answering, driver.Reply, "reply", "reply")
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
	case s.Transcript == nil:
		return s, transcript.Turn{}, fmt.Errorf("%w: the hooks of %s did not say where its transcript is", ErrNoReply, s.ID)
	case s.Turn == "":
		return s, transcript.Turn{}, fmt.Errorf("%w: %s has run no turn", ErrNoReply, s.ID)
	}
	deadline := time.Now().Add(transcriptLag)
	for {
		t, err := transcript.Last(ctx, s.Transcript.Path, s.Transcript.Format)
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

// Turns returns the last n turns of a session, in any state, as the agent's
// transcript holds them: the last one may be running. A session whose
// transcript is not written yet has none. tmag reads the transcript for this
// and keeps nothing of it.
func Turns(ctx context.Context, store session.Store, target string, n int) (Session, []transcript.Turn, error) {
	v, _, err := Load(ctx, store)
	if err != nil {
		return Session{}, nil, err
	}
	s, _, err := controlled(ctx, v, target)
	if err != nil {
		return Session{}, nil, err
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
