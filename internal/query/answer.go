package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
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
	v, _, err := prepare(ctx, log)
	if err != nil {
		return Asked{}, err
	}
	s, err := request(v, target, id)
	if err != nil {
		return Asked{}, err
	}
	switch driver.Channel(s.Agent, s.Runtime, driver.Answer) {
	case driver.Hook:
		if answer.Probe(ctx, log.Dir, id) == answer.Dead {
			return Asked{}, noHook(s, id)
		}
		ctx, cancel := context.WithTimeout(ctx, answerTimeout)
		defer cancel()
		a, err := answer.Show(ctx, log.Dir, id)
		if err != nil {
			return Asked{}, hookError(s, id, err)
		}
		return Asked{Session: s, Request: refOf(s, id), Asked: a}, nil
	case driver.Service:
		a, err := showService(ctx, v, s, id)
		if err != nil {
			return Asked{}, err
		}
		return Asked{Session: s, Request: refOf(s, id), Asked: a}, nil
	}
	return Asked{}, unsupported(s, driver.Answer)
}

// Answer decides an open request, by the channel the session's driver uses.
//
// A decision stands in for the person's, so @tmag-answer says who may give
// one: nobody, people, or agents too; no session answers its own. The
// request must still be open, checked again under the session's lock; the
// Answer event is logged before the lock is released. Delivery reaches the
// request and no other; a request the person answered in the terminal a
// moment before may take a decision the agent then ignores, which the
// events that follow show.
func Answer(ctx context.Context, log *eventlog.Log, target, id string, d answer.Decision) (Delivery, error) {
	v, caller, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	s, err := request(v, target, id)
	if err != nil {
		return Delivery{}, err
	}
	if err := caller.selfGate(s, "answer"); err != nil {
		return Delivery{}, err
	}
	if err := answerGate(ctx, s, caller); err != nil {
		return Delivery{}, err
	}
	switch driver.Channel(s.Agent, s.Runtime, driver.Answer) {
	case driver.Hook:
		return answerHook(ctx, log, s, id, d, caller)
	case driver.Service:
		return answerService(ctx, log, v, s, id, d, caller)
	}
	return Delivery{}, unsupported(s, driver.Answer)
}

// answerGate applies @tmag-answer to the caller.
func answerGate(ctx context.Context, s Session, c Caller) error {
	quick, cancel := context.WithTimeout(ctx, time.Second)
	mode := answer.Mode(quick)
	cancel()
	switch {
	case mode == answer.Off:
		return &RefusedError{RefuseUnsupported, fmt.Sprintf("answering is off (@tmag-answer); answer %s in the agent", s.ID)}
	case c.By != nil && mode != answer.Agents:
		return &RefusedError{RefuseAgent, fmt.Sprintf("a %s agent runs this command, and its decision would stand in for the person's; @tmag-answer lets agents answer when set to %s", c.By.Agent, answer.Agents)}
	}
	return nil
}

// answerHook hands the decision to the answer hook waiting on the request,
// under the session's lock: the hook prints it and leaves, the request
// closes, and what the agent then does reaches tmag through hooks that wait
// for the same lock.
func answerHook(ctx context.Context, log *eventlog.Log, s Session, id string, d answer.Decision, caller Caller) (Delivery, error) {
	if answer.Probe(ctx, log.Dir, id) == answer.Dead {
		return Delivery{}, noHook(s, id)
	}
	store := session.Store{Dir: log.Dir}
	var dl Delivery
	err := store.Lock(ctx, s.Agent, s.ID, func() error {
		rec, ok, err := store.Get(ctx, s.Agent, s.ID)
		if err != nil || !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
		}
		if _, open := rec.Request(id); !open {
			return &RefusedError{RefuseChanged, fmt.Sprintf("request %s of %s was answered or withdrawn", id, s.ID)}
		}
		decideCtx, cancel := context.WithTimeout(ctx, answerTimeout)
		defer cancel()
		if err := answer.Decide(decideCtx, log.Dir, id, d); err != nil {
			return hookError(s, id, err)
		}
		// The agent has the decision, and its next hooks wait for this lock:
		// the request is answered.
		after, _ := session.Apply(rec, adapter.Transition{Action: adapter.Answer, Request: id, Synthetic: true}, time.Now())
		ev := eventlog.Event{Event: eventlog.Answer, Via: driver.Hook, Request: id, Decision: d.Behavior}
		caller.stamp(&ev)
		if err := store.Report(ctx, log, after, &ev); err != nil {
			return fmt.Errorf("answer delivered but not logged: %w", err)
		}
		dl = Delivery{Session: s, Via: driver.Hook, Seq: ev.Seq}
		return nil
	})
	return dl, err
}

func noHook(s Session, id string) error {
	return &RefusedError{RefuseUnsupported, fmt.Sprintf("no answer hook waits on request %s of %s: answering was off (@tmag-answer) when it opened, or the hook gave up; answer it in the agent", id, s.ID)}
}

func hookError(s Session, id string, err error) error {
	if errors.Is(err, answer.ErrGone) {
		return &RefusedError{RefuseChanged, fmt.Sprintf("the answer hook of request %s of %s has left: %v", id, s.ID, err)}
	}
	return err
}

// openRequest is the request id as the session's record holds it.
func openRequest(v View, s Session, id string) (session.Request, session.Record, bool) {
	for _, r := range v.records {
		if r.Agent == s.Agent && r.ID == s.ID {
			if q, ok := r.Request(id); ok {
				return q, r, true
			}
		}
	}
	return session.Request{}, session.Record{}, false
}

// approvalOf is the daemon's approval request a Codex PermissionRequest
// hook's tool asks through, as Codex 0.166 maps them
// (core/src/tools/approvals.rs): shell commands, their stdin and network
// access as commands, patches as file changes. Other tools ask in ways tmag
// cannot answer.
func approvalOf(tool string) string {
	switch tool {
	case "Bash", "write_stdin":
		return codexsvc.CommandApproval
	case "apply_patch":
		return codexsvc.FileApproval
	}
	return ""
}

// pendingApproval joins the thread a request belongs to (a subagent's own,
// or the session's) and finds the daemon's approval request it is: the one
// of that thread and turn asking about the request's tool call, which Codex
// names by the call's id. A request stays open after its approval while the
// call runs, and several calls of one turn may ask, so nothing but the call
// binds an approval to a request; a request that may be about several calls
// is not answered here.
func pendingApproval(ctx context.Context, c *codexsvc.Client, s Session, q session.Request) (string, codexsvc.Message, error) {
	thread := s.ID
	if q.Prompt.Agent != "" {
		thread = q.Prompt.Agent
	}
	method := approvalOf(q.Prompt.Tool)
	if method == "" {
		return thread, codexsvc.Message{}, &RefusedError{RefuseUnsupported, fmt.Sprintf("request %s of %s asks about tool %q, which Codex approves in a way tmag cannot answer", q.ID, s.ID, q.Prompt.ToolName())}
	}
	if q.Call == "" {
		return thread, codexsvc.Message{}, fmt.Errorf("%w: request %s of %s may be about any of several %s calls; answer it in the agent", ErrAmbiguous, q.ID, s.ID, q.Prompt.ToolName())
	}
	pending, err := c.Join(ctx, thread)
	if err != nil {
		return thread, codexsvc.Message{}, err
	}
	var found []codexsvc.Message
	for _, m := range pending {
		if m.Method != method {
			continue
		}
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.ThreadID != thread || p.ItemID != q.Call || (q.Turn != "" && p.TurnID != q.Turn) {
			continue
		}
		found = append(found, m)
	}
	switch len(found) {
	case 0:
		return thread, codexsvc.Message{}, &RefusedError{RefuseChanged, fmt.Sprintf("the Codex daemon holds no approval of %s for request %s; it may have been answered", thread, q.ID)}
	case 1:
		return thread, found[0], nil
	}
	return thread, codexsvc.Message{}, fmt.Errorf("%w: the Codex daemon holds %d approvals of %s that request %s may be", ErrAmbiguous, len(found), thread, q.ID)
}

func showService(ctx context.Context, v View, s Session, id string) (answer.Asked, error) {
	q, rec, ok := openRequest(v, s, id)
	if !ok {
		return answer.Asked{}, &RefusedError{RefuseChanged, fmt.Sprintf("%s has no open request %s", s.ID, id)}
	}
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()
	c, err := reach(ctx, s, rec)
	if err != nil {
		return answer.Asked{}, err
	}
	defer c.Close()
	thread, m, err := pendingApproval(ctx, c, s, q)
	if err != nil {
		return answer.Asked{}, err
	}
	_ = c.Leave(ctx, thread)
	return answer.Asked{Tool: q.Prompt.ToolName(), Input: m.Params}, nil
}

// answerService answers the daemon's approval request a request is. tmag
// joins the thread for as long as it takes: the request is found and
// checked open again under the session's lock, Answer is logged, and the
// decision goes out after the lock is released (the daemon runs the
// session's hooks, which wait for it). The answer is done when the daemon
// reports the approval resolved.
func answerService(ctx context.Context, log *eventlog.Log, v View, s Session, id string, d answer.Decision, caller Caller) (Delivery, error) {
	decision := codexsvc.Accept
	switch {
	case d.Message != "":
		return Delivery{}, fmt.Errorf("%w: Codex takes no message with a decision", answer.ErrInvalid)
	case len(d.Answers) > 0:
		return Delivery{}, fmt.Errorf("%w: Codex approvals take no answers", answer.ErrInvalid)
	case d.Behavior == answer.Deny:
		decision = codexsvc.Decline
	}
	q, rec, ok := openRequest(v, s, id)
	if !ok {
		return Delivery{}, &RefusedError{RefuseChanged, fmt.Sprintf("%s has no open request %s", s.ID, id)}
	}
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()
	c, err := reach(ctx, s, rec)
	if err != nil {
		return Delivery{}, err
	}
	defer c.Close()
	thread, m, err := pendingApproval(ctx, c, s, q)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = c.Leave(ctx, thread) }()

	// The request is taken under the lock, so that of two answers at once
	// one is delivered and the other refused. It is free again once the
	// taker has given up: deliveries end within serviceTimeout.
	store := session.Store{Dir: log.Dir}
	var dl Delivery
	err = store.Lock(ctx, s.Agent, s.ID, func() error {
		rec, ok, err := store.Get(ctx, s.Agent, s.ID)
		if err != nil || !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
		}
		i := slices.IndexFunc(rec.Requests, func(o session.Request) bool { return o.ID == id })
		if i < 0 {
			return &RefusedError{RefuseChanged, fmt.Sprintf("request %s of %s was answered or withdrawn", id, s.ID)}
		}
		now := time.Now()
		if at := rec.Requests[i].Answered; now.Before(at.Add(serviceTimeout)) {
			return &RefusedError{RefuseChanged, fmt.Sprintf("request %s of %s is being answered", id, s.ID)}
		}
		rec.Requests = slices.Clone(rec.Requests)
		rec.Requests[i].Answered = now.UTC()
		ev := eventlog.Event{Event: eventlog.Answer, Via: driver.Service, Request: id, Decision: d.Behavior}
		caller.stamp(&ev)
		if err := store.Report(ctx, log, rec, &ev); err != nil {
			return err
		}
		dl = Delivery{Session: s, Via: driver.Service, Seq: ev.Seq}
		return nil
	})
	if err != nil {
		return Delivery{}, err
	}
	if err := c.Respond(ctx, m.ID, map[string]string{"decision": decision}); err != nil {
		return dl, fmt.Errorf("logged at seq %d, but the Codex daemon did not take it: %w", dl.Seq, err)
	}
	if err := c.AwaitResolved(ctx, m.ID); err != nil {
		return dl, fmt.Errorf("logged at seq %d and sent, but the Codex daemon did not report it resolved: %w", dl.Seq, err)
	}
	return dl, nil
}
