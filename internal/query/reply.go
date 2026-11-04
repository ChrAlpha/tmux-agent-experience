package query

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/transcript"
)

// ErrNoReply reports a session whose last turn left no reply to read.
var ErrNoReply = errors.New("no reply")

// transcriptLag is how long an agent may take to write a turn's end to its
// transcript after the hook that reported it.
const transcriptLag = 3 * time.Second

// LastReply returns the reply that ended an idle session's last turn, as
// the agent's transcript holds it: the turn is the one its hooks reported,
// never an earlier one the transcript ends with while the agent still
// writes. tmag reads the transcript for this and keeps nothing of it.
func LastReply(ctx context.Context, store session.Store, target string) (Session, transcript.Reply, error) {
	v, _, err := Load(ctx, store)
	if err != nil {
		return Session{}, transcript.Reply{}, err
	}
	s, _, err := controlled(v, target)
	if err != nil {
		return Session{}, transcript.Reply{}, err
	}
	switch {
	case s.State != adapter.Idle:
		return s, transcript.Reply{}, &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s; its turn has no final reply yet", s.ID, describe(s.State, s.Kind))}
	case s.Transcript == nil:
		return s, transcript.Reply{}, fmt.Errorf("%w: the hooks of %s did not say where its transcript is", ErrNoReply, s.ID)
	case s.Turn == "":
		return s, transcript.Reply{}, fmt.Errorf("%w: %s has run no turn", ErrNoReply, s.ID)
	}
	deadline := time.Now().Add(transcriptLag)
	for {
		r, err := transcript.Last(ctx, s.Transcript.Path, s.Transcript.Format)
		switch {
		case err == nil && r.Turn == s.Turn && r.Text == "":
			return s, r, fmt.Errorf("%w: turn %s of %s ended without one", ErrNoReply, s.Turn, s.ID)
		case err == nil && r.Turn == s.Turn:
			return s, r, nil
		case err != nil && !errors.Is(err, transcript.ErrNoTurn) && !errors.Is(err, fs.ErrNotExist):
			return s, transcript.Reply{}, err
		case time.Now().After(deadline):
			return s, transcript.Reply{}, fmt.Errorf("%w: the transcript of %s does not hold the end of its turn %s", ErrNoReply, s.ID, s.Turn)
		}
		select {
		case <-ctx.Done():
			return s, transcript.Reply{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
