package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/inbox"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// PendingWindow is how long a delivery holds off another without the agent
// reporting (see session.PendingWindow).
const PendingWindow = session.PendingWindow

// pending reports whether a prompt tmag submitted is still unconfirmed.
func pending(rec session.Record, now time.Time) bool {
	return !rec.Sent.IsZero() && now.Sub(rec.Sent) < PendingWindow
}

// queued reports whether a message tmag sent through the session's own
// channel is still unread by its main agent.
func queued(rec session.Record, now time.Time) bool {
	return !rec.Queued.IsZero() && now.Sub(rec.Queued) < PendingWindow
}

// Marker starts the first line tmag writes atop a prompt an agent sends:
// who sent it, and how to answer. No prompt given to tmag starts with it,
// so a prompt that did came from the agent the line names, as far as tmag
// delivered it.
const Marker = "[tmag]"

// ErrMarker refuses a prompt that starts with Marker: that line is tmag's.
var ErrMarker = errors.New("a prompt may not start with " + Marker + "; tmag writes that line")

// ErrNoSender reports a reply, or a prompt asking for one, from a caller
// that is no agent session tmag can prove: the reply would have nowhere to go.
var ErrNoSender = errors.New("only an agent whose session tmag proves sends or asks for a reply; run it from the agent's own tool")

// Ask is how a prompt an agent sends asks to be answered.
type Ask int

const (
	// AskFinal asks for the answer in the receiver's final message, which
	// the sender reads once the turn is over (read --last).
	AskFinal Ask = iota
	// AskReply asks the receiver to answer later with tmag reply.
	AskReply
	// Answering makes the prompt a reply, which nobody answers.
	Answering
	// AskNoAnswer tells the receiver that no answer is needed.
	AskNoAnswer
	// Notice tells an asker that the session it asked ended without
	// answering: tmag writes it on its own account.
	Notice
)

// header is the line tmag writes atop a prompt the caller sends: none for a
// person's, whose prompts are their own, nor for a notice, which is tmag's
// line alone.
func header(c Caller, ask Ask) (string, error) {
	switch {
	case ask == Notice:
		return "", nil
	case ask == AskNoAnswer:
		// A message says what it says; no answer is asked for.
		if c.By == nil {
			return "", nil
		}
	case ask != AskFinal && (c.By == nil || c.By.Session == ""):
		return "", ErrNoSender
	case c.By == nil:
		return "", nil
	}
	from := c.By.Agent
	if c.By.Session != "" {
		from += " " + c.By.Session
	}
	switch ask {
	case AskReply:
		return fmt.Sprintf("%s From %s, another agent. When done, send your answer on stdin to: tmag reply %s -", Marker, from, c.By.Session), nil
	case Answering:
		return fmt.Sprintf("%s Reply from %s, another agent, to your prompt. Do not reply to it.", Marker, from), nil
	case AskNoAnswer:
		return fmt.Sprintf("%s From %s, another agent. No answer needed.", Marker, from), nil
	}
	return fmt.Sprintf("%s From %s, another agent. Answer in your final message.", Marker, from), nil
}

// Send delivers text as a prompt to a session, asking for the answer ask
// names. Without a final answer an idle session starts a turn and a working
// one reads the prompt at its next step; AskFinal waits for an idle session
// and starts a turn of its own. When an agent sends it, tmag writes the
// first line: which agent and session sent it, and how to answer.
//
// The session must have nothing of tmag's unconfirmed, and not be the
// caller's own. The Send event is logged under the session's lock before the
// channel writes, so what the prompt causes is logged after it.
func Send(ctx context.Context, log *eventlog.Log, target, text string, ask Ask) (Delivery, error) {
	return deliver(ctx, log, target, prompt{text: text, ask: ask}, "send", "prompt")
}

// deliver carries a prompt for Send or Reply by the plan the driver makes,
// trying each channel in turn while the previous cannot be proven to reach
// the session. A channel is reached and proven first; the prompt is marked
// on the session and its Send event logged under the session's lock,
// against fresh state, before it is written.
func deliver(ctx context.Context, log *eventlog.Log, target string, p prompt, verb, selfVerb string) (Delivery, error) {
	p.text = strings.TrimRight(tmux.SanitizePrompt(p.text), "\n")
	if strings.TrimSpace(p.text) == "" {
		return Delivery{}, errors.New("nothing to " + verb)
	}
	if first, _, _ := strings.Cut(strings.TrimLeft(p.text, " \t\n"), "\n"); p.ask != Notice && strings.HasPrefix(strings.ToLower(first), Marker) {
		return Delivery{}, ErrMarker
	}
	v, w, caller, err := prepareWorld(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	if p.ask == Notice {
		caller = Caller{} // tmag speaks for itself, about an end it logged
	}
	line, err := header(caller, p.ask)
	if err != nil {
		return Delivery{}, err
	}
	if line != "" {
		p.text = line + "\n" + p.text
	}
	s, named, err := controlled(ctx, v, target)
	if err != nil {
		if p.ask == Answering {
			err = forgetAsker(ctx, log, v, caller, target, err)
		}
		return Delivery{}, err
	}
	if err := caller.selfGate(s, selfVerb); err != nil {
		return Delivery{}, err
	}
	p.caller, p.from = caller, driver.Person
	if caller.By != nil || p.ask == Notice {
		p.from = driver.Agent
	}
	switch p.ask {
	case AskFinal, AskReply:
		if p.owe, err = asking(ctx, v, caller, s, p.ask); err != nil {
			return Delivery{}, err
		}
	case Answering:
		claim, err := claimReply(ctx, log, w, caller, s)
		if err != nil {
			return Delivery{}, err
		}
		d, err := carry(ctx, log, v, s, named, p)
		return d, errors.Join(err, releaseReply(ctx, log, caller, claim, d, err))
	}
	return carry(ctx, log, v, s, named, p)
}

// prompt is one prompt on its way: its text with tmag's first line, who it
// speaks for and how it asks to be answered, the obligation it opens on the
// receiver, if any, and for tmag's notice the end it tells of.
type prompt struct {
	text   string
	caller Caller
	ask    Ask
	from   driver.Caller
	owe    *eventlog.Obligation
	notice int64
}

// mark records the prompt on the receiver's record, under its lock: the
// obligation it opens, under an id of its own, or the end its notice tells
// of.
func (p prompt) mark(rec *session.Record) {
	if p.owe != nil {
		rec.Owe(p.owe.To, p.owe.Kind, p.owe.Depth, time.Now())
	}
	if p.notice != 0 {
		rec.Notify(p.notice, time.Now())
	}
}

// carry delivers p to s by the plan the driver makes for it.
func carry(ctx context.Context, log *eventlog.Log, v View, s Session, named *Pane, p prompt) (Delivery, error) {
	idle, wait := s.State == adapter.Idle, p.ask == AskFinal
	gate, paneGate := liveGate, idleGate
	switch {
	case wait:
		gate = idleGate
	case p.ask == Answering || p.ask == Notice:
		gate = invitedGate
	}
	switch {
	case p.owe != nil:
		gate, paneGate = roomy(gate), roomy(paneGate)
	case p.notice != 0:
		gate, paneGate = untold(p.notice, gate), untold(p.notice, paneGate)
	}
	plan := driver.SendPlan(s.Agent, s.Runtime, p.from, wait, idle)
	var tried error
	for _, channel := range plan.Try {
		var d Delivery
		switch channel {
		case driver.Inbox:
			d, tried = throughInbox(ctx, log, v, s, p, gate)
		case driver.Pane:
			d, tried = throughPane(ctx, log, v, s, named, paneGate, panePrompt(p))
		case driver.Service:
			verb, started := servicePrompt(s, p, gate)
			d, tried = throughService(ctx, log, v, s, verb)
			d.Turn = *started
		}
		if !unbound(tried) {
			return d, tried
		}
	}
	switch {
	case plan.Busy && tried != nil:
		return Delivery{}, &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s and has no message channel tmag can prove", s.ID, describe(s.State, s.Kind))}
	case plan.Busy:
		return Delivery{}, &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s; tmag does not type into a busy pane", s.ID, describe(s.State, s.Kind))}
	case tried != nil:
		return Delivery{}, tried
	}
	return Delivery{}, unsupported(s, driver.Send)
}

// unbound reports a refusal that no channel tmag can prove reaches the
// session: the plan's next channel may.
func unbound(err error) bool {
	var refused *RefusedError
	return errors.As(err, &refused) && refused.Reason == RefuseUnbound
}

// servicePrompt is the Codex daemon's delivery of a prompt: a turn of its
// own starts on an idle thread; any other prompt starts an idle thread's
// turn or steers the active one, bound to the turn the fresh record names.
// started is the turn a delivery started, once it has.
func servicePrompt(s Session, p prompt, gate stateGate) (verb serviceVerb, started *string) {
	active, turn, wait := false, "", p.ask == AskFinal
	started = new(string)
	return serviceVerb{
		gate: gate,
		check: func(ctx context.Context, c *codexsvc.Client) error {
			if wait {
				return threadIn(ctx, c, s, RefuseBusy, codexsvc.StatusIdle)
			}
			st, err := threadStatus(ctx, c, s)
			if err != nil {
				return err
			}
			switch st {
			case codexsvc.StatusIdle:
				return nil
			case codexsvc.StatusActive:
				active = true
				return nil
			}
			return &RefusedError{RefuseChanged, fmt.Sprintf("the Codex daemon has %s %s", s.ID, st)}
		},
		mark: func(rec *session.Record) eventlog.Event {
			// A prompt into a turn is queued until a main-agent report
			// confirms it, like one on the session's own channel. A wait
			// leaves the prompt it started unconfirmed instead.
			if wait {
				rec.Sent = time.Now().UTC()
			} else {
				rec.Queued = time.Now().UTC()
			}
			if active {
				turn = rec.Turn
			}
			p.mark(rec)
			return sentEvent(*rec, p)
		},
		deliver: func(ctx context.Context, c *codexsvc.Client) error {
			if active {
				return c.SteerTurn(ctx, s.ID, turn, p.text)
			}
			var err error
			*started, err = c.StartTurn(ctx, s.ID, p.text)
			return err
		},
	}, started
}

// panePrompt is the pane delivery of a prompt: it marks the prompt sent
// before typing, so another sender is held off even if typing fails after the
// prompt went out, and logs Send before the agent's hooks can report the turn.
// A prompt that opens an obligation reports the session it marked.
func panePrompt(pr prompt) paneVerb {
	return paneVerb{
		plan: func(_ Session, _ Pane, rec session.Record) (session.Record, eventlog.Event, *session.Record) {
			before := rec.Reports()
			rec.Sent = time.Now().UTC()
			pr.mark(&rec)
			var after *session.Record
			if rec.Reports() != before {
				after = &rec
			}
			return rec, sentEvent(rec, pr), after
		},
		press: func(ctx context.Context, s Session, p Pane) error {
			if err := (tmux.Server{Socket: p.Server}).Submit(ctx, p.ID, pr.text); err != nil {
				return fmt.Errorf("typing into %s failed, perhaps after the prompt was delivered; %s takes no other prompt for %s: %w",
					p.ID, s.ID, PendingWindow, err)
			}
			return nil
		},
	}
}

// sentEvent is the Send event of a prompt, before its channel writes it.
func sentEvent(rec session.Record, p prompt) eventlog.Event {
	ev := eventlog.Event{Event: eventlog.Send, Cwd: rec.Cwd, Bytes: len(p.text), Notice: p.notice}
	p.caller.stamp(&ev)
	return ev
}

// throughInbox delivers text on the socket the session's own agent serves
// for messages. The socket is dialled and proven before anything is logged,
// so a message is only recorded for a channel that reaches the session.
func throughInbox(outer context.Context, log *eventlog.Log, v View, s Session, p prompt, gate stateGate) (Delivery, error) {
	if err := gateOf(v, s, gate); err != nil {
		return Delivery{}, err
	}
	store := session.Store{Dir: log.Dir}
	rec, ok, err := store.Get(outer, s.Agent, s.ID)
	if err != nil || !ok {
		return Delivery{}, sessionEnded(s.ID)
	}
	ctx, cancel := context.WithTimeout(outer, serviceTimeout)
	defer cancel()
	c, err := inboxAt(ctx, s, rec)
	if err != nil {
		return Delivery{}, err
	}
	defer c.Close()

	// The envelope's class and the hold warning are what tmag proves at the
	// delivery decision, after recordDelivery re-checks the receiver under
	// its lock and reads the sender's record afresh. A later hook may change
	// either class; the frame still attests what was true then.
	var fromMode string
	var held bool
	d, undo, project, err := recordDelivery(ctx, log, s, gate, driver.Inbox, rec.Inbox, func() error {
		return sessionEnded(s.ID)
	}, func(rec *session.Record) eventlog.Event {
		// A wait asks for a turn of its own: only a turn starting clears its
		// Sent mark. A late report of the previous turn clears neither it nor
		// the wait. The asynchronous modes are queued until the main agent's
		// next report.
		if p.ask == AskFinal {
			rec.Sent = time.Now().UTC()
		} else {
			rec.Queued = time.Now().UTC()
		}
		p.mark(rec)
		return sentEvent(*rec, p)
	}, func(receiver session.Record) {
		fromMode, held = claudeDelivery(ctx, store, receiver, p)
	})
	defer project(outer)
	if err != nil {
		return Delivery{}, err
	}
	if err := outer.Err(); err != nil {
		return undone(outer, d, &NotTakenError{Seq: d.Seq, Target: s.ID, Err: err}, undo) // nothing written
	}
	ctx, cancelDelivery := context.WithTimeout(outer, serviceTimeout)
	defer cancelDelivery()
	if s.Agent == proc.Claude {
		d.Held = held
		err = c.Claude(ctx, inbox.ClaudeMessage{Name: senderName(p.caller), Mode: fromMode, Session: rec.ID, Text: p.text})
	} else {
		err = c.Extension(ctx, p.text, s.ID)
	}
	if err != nil {
		return undone(outer, d, inboxFailure(d, err), undo)
	}
	return d, nil
}

// claudeDelivery decides a Claude delivery's permission class and whether the
// receiver will hold it, from the receiver's record read under its lock: a
// person or owner client speaks with the receiver's class, an agent with its
// own class as its record, read afresh, says, and tmag's notice with none.
func claudeDelivery(ctx context.Context, store session.Store, receiver session.Record, p prompt) (mode string, held bool) {
	if p.ask != Notice {
		mode = claudeMode(ctx, store, receiver, p.caller)
	}
	held = receiver.Permission == adapter.PermissionBypass && mode != adapter.PermissionBypass
	return mode, held
}

// claudeMode is the permission class tmag can attest for a sender to Claude
// Code: a person or owner client speaks with the receiver's own class, and an
// agent with its own recorded class, when its session is known.
func claudeMode(ctx context.Context, store session.Store, rec session.Record, caller Caller) string {
	if caller.By == nil {
		return rec.Permission
	}
	if caller.By.Session == "" {
		return ""
	}
	sender, ok, err := store.Get(ctx, caller.By.Agent, caller.By.Session)
	if err != nil || !ok {
		return ""
	}
	return sender.Permission
}

// inboxFailure reports a failed inbox write: an explicit refusal or a
// process serving another session proves nothing was delivered; anything
// else leaves the outcome unknown.
func inboxFailure(d Delivery, err error) error {
	if errors.Is(err, inbox.ErrNotTaken) {
		return &NotTakenError{Seq: d.Seq, Target: d.Session.ID, Err: err}
	}
	var refused *inbox.Refusal
	if errors.As(err, &refused) {
		reason := RefuseChanged
		switch refused.Reason {
		case RefuseIdle, RefuseUnsupported:
			reason = refused.Reason
		}
		return &RefusedError{reason, refused.Error()}
	}
	return unknownDelivery(d, err)
}

// inboxAt connects to the inbox a session's hooks recorded and proves the
// connection reaches it: its peer is the process that named the socket, and
// that process still runs as it did.
func inboxAt(ctx context.Context, s Session, rec session.Record) (*inbox.Conn, error) {
	if rec.Inbox == nil {
		return nil, &RefusedError{RefuseUnbound, fmt.Sprintf("%s has no inbox tmag can prove", s.ID)}
	}
	c, err := inbox.Dial(ctx, rec.Inbox.Path)
	if err != nil {
		return nil, &RefusedError{RefuseUnbound, fmt.Sprintf("nothing serves the inbox %s of %s: %v", rec.Inbox.Path, s.ID, err)}
	}
	if !c.Serves(ctx, *rec.Inbox) {
		c.Close()
		return nil, &RefusedError{RefuseUnbound, fmt.Sprintf("%s is served by process %d, which does not run %s", rec.Inbox.Path, c.PeerPID, s.ID)}
	}
	return c, nil
}

// liveGate admits a send to a live session with nothing of tmag's still
// unconfirmed. Any state takes it: the session's own channel queues it.
func liveGate(rec session.Record, now time.Time) error { return unconfirmed(rec, now) }

// invitedGate admits a reply or a notice, which the receiver's own ask
// invited, and whose number its obligations bound: a message tmag queued
// before does not hold it off, while a prompt tmag sent that no turn has
// confirmed still does.
func invitedGate(rec session.Record, now time.Time) error {
	if pending(rec, now) {
		return unconfirmed(rec, now)
	}
	return nil
}

// idleGate admits an idle session with nothing of tmag's unconfirmed.
func idleGate(rec session.Record, now time.Time) error {
	if rec.State() != adapter.Idle {
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s", rec.ID, describe(rec.State(), rec.Kind()))}
	}
	return liveGate(rec, now)
}

// unconfirmed reports what tmag last submitted that the session has not
// confirmed, if it was recent enough to hold another delivery off.
func unconfirmed(rec session.Record, now time.Time) error {
	switch {
	case pending(rec, now):
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s has not started the prompt sent %s ago", rec.ID, now.Sub(rec.Sent).Round(time.Second))}
	case queued(rec, now):
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s has not reached its next step since the prompt sent %s ago", rec.ID, now.Sub(rec.Queued).Round(time.Second))}
	}
	return nil
}

// senderName names the sender in Claude Code's envelope: tmag for a person
// or an owner client, and an agent with a proven session by its name and the
// first eight characters of its id.
func senderName(c Caller) string {
	if c.By == nil || c.By.Session == "" {
		return "tmag"
	}
	id := c.By.Session
	if len(id) > 8 {
		id = id[:8]
	}
	return c.By.Agent + " " + id + " via tmag"
}

// sessionEnded reports a session that ended between being resolved and
// being sent the prompt.
func sessionEnded(id string) error { return fmt.Errorf("%w: %s", ErrEnded, id) }

// NotTakenError reports a message tmag logged that the agent's channel then
// refused before taking: nothing was delivered, so the command may run
// again.
type NotTakenError struct {
	Seq    int64
	Target string
	Err    error
}

func (e *NotTakenError) Error() string {
	return fmt.Sprintf("logged at seq %d, but %s did not take it: %v", e.Seq, e.Target, e.Err)
}

func (e *NotTakenError) Unwrap() error { return e.Err }
