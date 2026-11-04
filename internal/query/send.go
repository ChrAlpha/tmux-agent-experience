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
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// PendingWindow is how long a submitted prompt holds off further sends
// without a turn confirming it. Input that starts no turn (a slash command,
// say) releases the session when it passes.
const PendingWindow = 15 * time.Second

func pending(rec session.Record, now time.Time) bool {
	return !rec.Sent.IsZero() && now.Sub(rec.Sent) < PendingWindow
}

// Marker starts the first line tmag writes atop a prompt an agent sends:
// who sent it, and how to answer. No prompt given to send starts with it,
// so a prompt that does came from the agent the line names, as far as tmag
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
)

// header is the line tmag writes atop a prompt the caller sends: none for a
// person's, whose prompts are their own.
func header(c Caller, ask Ask) (string, error) {
	switch {
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
	}
	return fmt.Sprintf("%s From %s, another agent. Answer in your final message.", Marker, from), nil
}

// Send delivers text as one submitted prompt to an idle session, by the
// channel its driver uses, and returns where it went. When an agent sends
// it, tmag writes the first line: which agent and session sent it, and how
// ask has it answered.
//
// The session must be idle with no prompt of tmag's still unconfirmed, and
// not the caller's own. The Send event is logged under the session's lock,
// which the session's hooks wait for, so the turn the prompt starts is
// logged after it; until a hook confirms that turn, further sends are
// refused.
func Send(ctx context.Context, log *eventlog.Log, target, text string, ask Ask) (Delivery, error) {
	text = strings.TrimRight(tmux.SanitizePrompt(text), "\n")
	if strings.TrimSpace(text) == "" {
		return Delivery{}, errors.New("nothing to send")
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
	s, named, err := controlled(v, target)
	if err != nil {
		return Delivery{}, err
	}
	if err := caller.selfGate(s, "prompt"); err != nil {
		return Delivery{}, err
	}
	switch driver.Channel(s.Agent, s.Runtime, driver.Send) {
	case driver.Service:
		return throughService(ctx, log, v, s, serviceVerb{
			gate: idleGate,
			check: func(ctx context.Context, c *codexsvc.Client) error {
				return threadIn(ctx, c, s, RefuseBusy, codexsvc.StatusIdle)
			},
			mark: func(rec *session.Record) eventlog.Event {
				rec.Sent = time.Now().UTC().Truncate(time.Second)
				ev := eventlog.Event{Event: eventlog.Send, Cwd: rec.Cwd, Bytes: len(text)}
				caller.stamp(&ev)
				return ev
			},
			deliver: func(ctx context.Context, c *codexsvc.Client) error {
				_, err := c.StartTurn(ctx, s.ID, text)
				return err
			},
		})
	case driver.Pane:
		store := session.Store{Dir: log.Dir}
		return throughPane(ctx, log, v, s, named, idleGate, func(ctx context.Context, s Session, p Pane, rec session.Record) (eventlog.Event, *session.Record, error) {
			// Hold off other senders before typing: a prompt recorded as sent
			// but never delivered costs a pending window, a prompt delivered
			// but not recorded could be delivered twice. A failure while
			// typing does not say whether tmux typed it, so the mark stays.
			rec.Sent = time.Now().UTC().Truncate(time.Second)
			if err := store.Put(rec); err != nil {
				return eventlog.Event{}, nil, err
			}
			if err := (tmux.Server{Socket: p.Server}).Submit(ctx, p.ID, text); err != nil {
				return eventlog.Event{}, nil, fmt.Errorf("typing into %s failed, perhaps after the prompt was delivered; %s takes no other prompt for %s: %w",
					p.ID, s.ID, PendingWindow, err)
			}
			ev := eventlog.Event{Event: eventlog.Send, Cwd: p.Cwd, Bytes: len(text)}
			caller.stamp(&ev)
			return ev, nil, nil
		})
	}
	return Delivery{}, unsupported(s, driver.Send)
}

// idleGate admits an idle session with no prompt of tmag's unconfirmed.
func idleGate(s Session, v View, now time.Time) error {
	if s.State != adapter.Idle {
		return &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s", s.ID, describe(s.State, s.Kind))}
	}
	for _, r := range v.records {
		if r.Agent == s.Agent && r.ID == s.ID && pending(r, now) {
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s has not started the prompt sent %s ago", s.ID, now.Sub(r.Sent).Round(time.Second))}
		}
	}
	return nil
}
