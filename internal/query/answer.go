package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// answerTimeout bounds handing a decision to an answer hook.
const answerTimeout = 5 * time.Second

// Asked is an open request and what it asks.
type Asked struct {
	Session Session
	Request eventlog.Request
	answer.Asked
}

// request finds the open request id of the session target names.
func request(v View, target, id string) (Session, error) {
	s, _, err := controlled(v, target)
	if err != nil {
		return Session{}, err
	}
	for _, r := range s.Requests {
		if r.ID == id {
			return s, nil
		}
	}
	return Session{}, &RefusedError{RefuseChanged, fmt.Sprintf("%s has no open request %s", s.ID, id)}
}

func refOf(s Session, id string) eventlog.Request {
	for _, r := range s.Requests {
		if r.ID == id {
			return r
		}
	}
	return eventlog.Request{ID: id}
}

// Show returns what an open request asks: the tool and its input, relayed
// from where the agent holds them and never stored.
func Show(ctx context.Context, log *eventlog.Log, target, id string) (Asked, error) {
	v, err := prepare(ctx, log)
	if err != nil {
		return Asked{}, err
	}
	s, err := request(v, target, id)
	if err != nil {
		return Asked{}, err
	}
	if driver.Channel(s.Agent, s.Runtime, driver.Answer) == driver.Hook {
		ctx, cancel := context.WithTimeout(ctx, answerTimeout)
		defer cancel()
		a, err := answer.Show(ctx, log.Dir, id)
		if err != nil {
			return Asked{}, hookError(s, id, err)
		}
		return Asked{Session: s, Request: refOf(s, id), Asked: a}, nil
	}
	return Asked{}, unsupported(s, driver.Answer)
}

// Answer decides an open request, by the channel the session's driver uses.
//
// The request must still be open, checked again under the session's lock;
// the Answer event is logged before the lock is released. Delivery reaches
// the request and no other; a request the person answered in the terminal a
// moment before may take a decision the agent then ignores, which the
// events that follow show.
func Answer(ctx context.Context, log *eventlog.Log, target, id string, d answer.Decision) (Delivery, error) {
	v, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	s, err := request(v, target, id)
	if err != nil {
		return Delivery{}, err
	}
	if driver.Channel(s.Agent, s.Runtime, driver.Answer) == driver.Hook {
		return answerHook(ctx, log, s, id, d)
	}
	return Delivery{}, unsupported(s, driver.Answer)
}

// answerHook hands the decision to the answer hook waiting on the request,
// under the session's lock: the hook prints it and leaves, and what the
// agent then does reaches tmag through hooks that wait for the same lock.
func answerHook(ctx context.Context, log *eventlog.Log, s Session, id string, d answer.Decision) (Delivery, error) {
	if !answer.Waiting(log.Dir, id) {
		return Delivery{}, &RefusedError{RefuseUnsupported, fmt.Sprintf("no answer hook waits on request %s of %s: answering is off (@tmag-answer), or the hook gave up", id, s.ID)}
	}
	store := session.Store{Dir: log.Dir}
	var dl Delivery
	err := store.Lock(ctx, s.Agent, s.ID, func() error {
		rec, ok, err := store.Get(s.Agent, s.ID)
		if err != nil || !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
		}
		if _, open := rec.Request(id); !open {
			return &RefusedError{RefuseChanged, fmt.Sprintf("request %s of %s was answered or withdrawn", id, s.ID)}
		}
		ctx, cancel := context.WithTimeout(ctx, answerTimeout)
		defer cancel()
		if err := answer.Decide(ctx, log.Dir, id, d); err != nil {
			return hookError(s, id, err)
		}
		ev := eventlog.Event{Agent: s.Agent, Session: s.ID, Event: eventlog.Answer, Cwd: rec.Cwd, Via: driver.Hook, Request: id, Decision: d.Behavior}
		if err := log.Append(ctx, &ev); err != nil {
			return fmt.Errorf("answer delivered but not logged: %w", err)
		}
		dl = Delivery{Session: s, Via: driver.Hook, Seq: ev.Seq}
		return nil
	})
	return dl, err
}

func hookError(s Session, id string, err error) error {
	if errors.Is(err, answer.ErrGone) {
		return &RefusedError{RefuseChanged, fmt.Sprintf("the answer hook of request %s of %s has left: %v", id, s.ID, err)}
	}
	return err
}
