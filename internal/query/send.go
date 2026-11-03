package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
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

// Send delivers text as one submitted prompt to an idle session, by the
// channel its driver uses, and returns where it went.
//
// The session must be idle with no prompt of tmag's still unconfirmed. The
// Send event is logged under the session's lock, which the session's hooks
// wait for, so the turn the prompt starts is logged after it; until a hook
// confirms that turn, further sends are refused.
func Send(ctx context.Context, log *eventlog.Log, target, text string, from *eventlog.PaneRef) (Delivery, error) {
	text = strings.TrimRight(tmux.SanitizePrompt(text), "\n")
	if strings.TrimSpace(text) == "" {
		return Delivery{}, errors.New("nothing to send")
	}
	v, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	s, named, err := controlled(v, target)
	if err != nil {
		return Delivery{}, err
	}
	if driver.Channel(s.Agent, s.Runtime, driver.Send) == driver.Pane {
		store := session.Store{Dir: log.Dir}
		return throughPane(ctx, log, v, s, named, idleGate, func(ctx context.Context, s Session, p Pane, rec session.Record) (eventlog.Event, error) {
			// Hold off other senders before typing: a prompt recorded as sent
			// but never delivered costs a pending window, a prompt delivered
			// but not recorded could be delivered twice. A failure while
			// typing does not say whether tmux typed it, so the mark stays.
			rec.Sent = time.Now().UTC().Truncate(time.Second)
			if err := store.Put(rec); err != nil {
				return eventlog.Event{}, err
			}
			if err := (tmux.Server{Socket: p.Server}).Submit(ctx, p.ID, text); err != nil {
				return eventlog.Event{}, fmt.Errorf("typing into %s failed, perhaps after the prompt was delivered; %s takes no other prompt for %s: %w",
					p.ID, s.ID, PendingWindow, err)
			}
			return eventlog.Event{Event: eventlog.Send, Cwd: p.Cwd, From: from, Bytes: len(text)}, nil
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
