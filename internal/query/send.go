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
// reporting: a prompt until a turn confirms it, a message until the main
// agent reaches its next step. Input that starts no turn (a slash command,
// say) releases the session when it passes.
const PendingWindow = 15 * time.Second

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
)

// driverMode is how the driver sees the response the caller asks for.
func driverMode(ask Ask) driver.Mode {
	switch ask {
	case AskFinal:
		return driver.Wait
	case AskReply, Answering:
		return driver.Reply
	}
	return driver.Now
}

// header is the line tmag writes atop a prompt the caller sends: none for a
// person's, whose prompts are their own.
func header(c Caller, ask Ask) (string, error) {
	switch {
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
	return deliver(ctx, log, target, text, ask, driverMode(ask), "send", "prompt")
}

// deliver carries a prompt for Send or Reply by the route the driver chooses.
// The channel is reached and proven first; the prompt is marked on the
// session and its Send event logged under the session's lock, against fresh
// state, before it is written.
func deliver(ctx context.Context, log *eventlog.Log, target, text string, ask Ask, mode driver.Mode, verb, selfVerb string) (Delivery, error) {
	text = strings.TrimRight(tmux.SanitizePrompt(text), "\n")
	if strings.TrimSpace(text) == "" {
		return Delivery{}, errors.New("nothing to " + verb)
	}
	if first, _, _ := strings.Cut(strings.TrimLeft(text, " \t\n"), "\n"); strings.HasPrefix(strings.ToLower(first), Marker) {
		return Delivery{}, ErrMarker
	}
	v, caller, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	line, err := header(caller, ask)
	if err != nil {
		return Delivery{}, err
	}
	if line != "" {
		text = line + "\n" + text
	}
	s, named, err := controlled(ctx, v, target)
	if err != nil {
		return Delivery{}, err
	}
	if err := caller.selfGate(s, selfVerb); err != nil {
		return Delivery{}, err
	}
	idle := s.State == adapter.Idle
	from := driver.Person
	if caller.By != nil {
		from = driver.Agent
	}
	route := driver.SendRoute(s.Agent, s.Runtime, from, mode, idle)
	gate := liveGate
	if mode == driver.Wait {
		gate = idleGate
	}
	switch route.Channel {
	case driver.Inbox:
		d, err := throughInbox(ctx, log, v, s, text, caller, gate, mode)
		if err == nil {
			return d, nil
		}
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != RefuseUnbound {
			return Delivery{}, err
		}
		if route.Fallback == driver.Pane {
			return throughPane(ctx, log, v, s, named, idleGate, panePrompt(text, caller))
		}
		if refusal := noMessageChannel(s, from, idle); refusal != nil {
			return Delivery{}, refusal
		}
		return Delivery{}, err
	case driver.Pane:
		return throughPane(ctx, log, v, s, named, idleGate, panePrompt(text, caller))
	case driver.Service:
		return throughService(ctx, log, v, s, serviceVerb{
			gate: gate,
			check: func(ctx context.Context, c *codexsvc.Client) error {
				if mode == driver.Wait {
					return threadIn(ctx, c, s, RefuseBusy, codexsvc.StatusIdle)
				}
				return threadIn(ctx, c, s, RefuseChanged, codexsvc.StatusIdle, codexsvc.StatusActive)
			},
			mark: func(rec *session.Record) eventlog.Event {
				// turn/start takes a prompt into the thread's turn when the
				// daemon answers; until a main-agent report confirms it, it
				// is queued like a prompt on the session's own channel. A
				// wait leaves the prompt it started unconfirmed instead.
				if mode == driver.Wait {
					rec.Sent = time.Now().UTC().Truncate(time.Second)
				} else {
					rec.Queued = time.Now().UTC().Truncate(time.Second)
				}
				return sentEvent(*rec, text, caller)
			},
			deliver: func(ctx context.Context, c *codexsvc.Client) error {
				// turn/start joins an active turn and starts an idle one.
				_, err := c.StartTurn(ctx, s.ID, text)
				return err
			},
		})
	}
	if !idle && hasPane(s.Agent, s.Runtime) {
		return Delivery{}, &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s; tmag does not type into a busy pane", s.ID, describe(s.State, s.Kind))}
	}
	return Delivery{}, unsupported(s, driver.Send)
}

// panePrompt is the pane delivery of a prompt: it marks the prompt sent
// before typing, so another sender is held off even if typing fails after the
// prompt went out, and logs Send before the agent's hooks can report the turn.
func panePrompt(text string, caller Caller) paneVerb {
	return paneVerb{
		plan: func(_ Session, p Pane, rec session.Record) (session.Record, eventlog.Event, *session.Record) {
			rec.Sent = time.Now().UTC().Truncate(time.Second)
			return rec, sentEvent(rec, text, caller), nil
		},
		press: func(ctx context.Context, s Session, p Pane) error {
			if err := (tmux.Server{Socket: p.Server}).Submit(ctx, p.ID, text); err != nil {
				return fmt.Errorf("typing into %s failed, perhaps after the prompt was delivered; %s takes no other prompt for %s: %w",
					p.ID, s.ID, PendingWindow, err)
			}
			return nil
		},
	}
}

// sentEvent is the Send event of a prompt, before its channel writes it.
func sentEvent(rec session.Record, text string, caller Caller) eventlog.Event {
	ev := eventlog.Event{Event: eventlog.Send, Cwd: rec.Cwd, Bytes: len(text)}
	caller.stamp(&ev)
	return ev
}

// throughInbox delivers text on the socket the session's own agent serves
// for messages. The socket is dialled and proven before anything is logged,
// so a message is only recorded for a channel that reaches the session.
func throughInbox(outer context.Context, log *eventlog.Log, v View, s Session, text string, caller Caller, gate stateGate, mode driver.Mode) (Delivery, error) {
	if err := gate(s, v, time.Now()); err != nil {
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

	sent := time.Now().UTC().Truncate(time.Second)
	d, err := recordDelivery(ctx, log, s, gate, driver.Inbox, rec.Inbox, func() error {
		return sessionEnded(s.ID)
	}, func(rec *session.Record) eventlog.Event {
		// A wait asks for a turn of its own: only a turn starting clears its
		// Sent mark. A late report of the previous turn clears neither it nor
		// the wait. The asynchronous modes are queued until the main agent's
		// next report.
		if mode == driver.Wait {
			rec.Sent = sent
		} else {
			rec.Queued = sent
		}
		return sentEvent(*rec, text, caller)
	})
	if err != nil {
		return Delivery{}, err
	}
	ctx, cancelDelivery := context.WithTimeout(outer, serviceTimeout)
	defer cancelDelivery()
	if s.Agent == proc.Claude {
		err = c.Claude(ctx, senderName(caller), text)
	} else {
		err = c.Extension(ctx, text, s.ID)
	}
	if err != nil {
		return d, inboxFailure(d, err)
	}
	return d, nil
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
		return &RefusedError{RefuseChanged, refused.Error()}
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

// noMessageChannel says what an unproven inbox means for a caller: a person
// working, or an agent working a terminal session, may try once it is idle;
// a background session has no pane to fall back to.
func noMessageChannel(s Session, from driver.Caller, idle bool) error {
	if from == driver.Person {
		if !idle {
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s and has no message channel tmag can prove", s.ID, describe(s.State, s.Kind))}
		}
		return nil
	}
	if !idle && hasPane(s.Agent, s.Runtime) {
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s and has no message channel tmag can prove", s.ID, describe(s.State, s.Kind))}
	}
	return nil
}

// hasPane reports whether a session of this agent and runtime can be typed
// into at all, idle or not.
func hasPane(agent, runtime string) bool {
	if runtime == "service" {
		return false
	}
	switch agent {
	case proc.Claude, proc.Codex, proc.Pi, proc.OMP:
		return true
	}
	return false
}

// liveGate admits a send to a live session with nothing of tmag's still
// unconfirmed. Any state takes it: the session's own channel queues it.
func liveGate(s Session, v View, now time.Time) error {
	for _, r := range v.records {
		if r.Agent == s.Agent && r.ID == s.ID {
			if err := unconfirmed(r, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// idleGate admits an idle session with nothing of tmag's unconfirmed.
func idleGate(s Session, v View, now time.Time) error {
	if s.State != adapter.Idle {
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s", s.ID, describe(s.State, s.Kind))}
	}
	return liveGate(s, v, now)
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
