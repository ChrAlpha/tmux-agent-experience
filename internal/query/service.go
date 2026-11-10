package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// serviceTimeout bounds each exchange with an agent service. An interrupt of
// a steered Codex turn was seen never to be answered.
const serviceTimeout = 5 * time.Second

// reach connects to the agent service a session runs in and proves it is
// that service: the socket's peer is the session's host, or an ancestor of
// it, since hooks may run in a helper the daemon started.
func reach(ctx context.Context, s Session, rec session.Record) (*codexsvc.Client, error) {
	host := rec.Host
	if host.PID == 0 || !proc.RunningContext(ctx, host.PID, host.Start) {
		return nil, &RefusedError{RefuseChanged, fmt.Sprintf("the service %s ran in has exited", s.ID)}
	}
	sock := codexsvc.Socket()
	c, err := codexsvc.Dial(ctx, sock)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Codex daemon at %s: %w", sock, err)
	}
	t, err := proc.SnapshotContext(ctx)
	if err != nil {
		c.Close()
		return nil, err
	}
	if p, ok := t.Get(host.PID); ok && !p.Uncertain && (host.Start == "" || p.Start == host.Start) {
		if slices.Contains(t.Ancestors(host.PID), c.PeerPID) {
			return c, nil
		}
	}
	c.Close()
	return nil, &RefusedError{RefuseUnbound, fmt.Sprintf("%s is served by process %d, which does not run %s", sock, c.PeerPID, s.ID)}
}

// serviceVerb is one verb carried by an agent service.
type serviceVerb struct {
	gate stateGate
	// check asks the service whether it agrees, before anything is logged.
	check func(ctx context.Context, c *codexsvc.Client) error
	// mark records the verb on the session, under its lock, and returns the
	// event to log.
	mark func(rec *session.Record) eventlog.Event
	// deliver carries the verb, after the lock is released.
	deliver func(ctx context.Context, c *codexsvc.Client) error
}

// throughService delivers a verb through the agent service s runs in.
// tmag's gate comes first; then the service is reached, proven and asked
// whether it agrees. The verb is marked on the session and logged under the
// session's lock, against fresh state, and delivered after the lock is
// released: the service may run the session's hooks, which wait for that
// lock, before it answers.
func throughService(outer context.Context, log *eventlog.Log, v View, s Session, verb serviceVerb) (Delivery, error) {
	if err := verb.gate(s, v, time.Now()); err != nil {
		return Delivery{}, err
	}
	store := session.Store{Dir: log.Dir}
	rec, ok, err := store.Get(outer, s.Agent, s.ID)
	if err != nil || !ok {
		return Delivery{}, &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
	}
	ctx, cancel := context.WithTimeout(outer, serviceTimeout)
	defer cancel()
	c, err := reach(ctx, s, rec)
	if err != nil {
		return Delivery{}, err
	}
	defer c.Close()
	if err := verb.check(ctx, c); err != nil {
		return Delivery{}, err
	}

	var d Delivery
	d, err = recordDelivery(ctx, log, s, verb.gate, driver.Service, nil, func() error {
		return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
	}, verb.mark)
	if err != nil {
		return Delivery{}, err
	}
	ctx, cancelDelivery := context.WithTimeout(outer, serviceTimeout)
	defer cancelDelivery()
	if err := verb.deliver(ctx, c); err != nil {
		return d, deliveryFailure(d, err)
	}
	return d, nil
}

// deliveryFailure reports a failed write after its event was logged. A
// JSON-RPC error response from the Codex daemon refuses the request, which
// proves nothing was taken; anything else leaves the outcome unknown.
func deliveryFailure(d Delivery, err error) error {
	var rpc *codexsvc.RPCError
	if errors.As(err, &rpc) {
		return &NotTakenError{Seq: d.Seq, Target: d.Session.ID, Err: err}
	}
	return unknownDelivery(d, err)
}

// unknownDelivery reports a write whose outcome tmag cannot know.
func unknownDelivery(d Delivery, err error) error {
	return fmt.Errorf("logged at seq %d; whether %s took it is unknown: %w", d.Seq, d.Session.ID, err)
}

// recordDelivery marks a delivery on the session under its lock, against
// fresh state, and logs the event before releasing it: the session's hooks
// wait for the same lock, so what the delivery causes is logged after it.
// The channel is reached and checked already, so only the session's own gate
// runs again; proven, when not nil, is the inbox that channel was proven on.
// gone says what a session that ended meanwhile is.
func recordDelivery(ctx context.Context, log *eventlog.Log, s Session, gate stateGate, via string, proven *session.Inbox, gone func() error, mark func(*session.Record) eventlog.Event) (Delivery, error) {
	store := session.Store{Dir: log.Dir}
	var d Delivery
	err := store.Lock(ctx, s.Agent, s.ID, func() error {
		fresh, _, err := Load(ctx, store)
		if err != nil {
			return err
		}
		fs, ok := fresh.Session(s.Agent, s.ID)
		if !ok {
			return gone()
		}
		if err := gate(fs, fresh, time.Now()); err != nil {
			return err
		}
		rec, ok, err := store.Get(ctx, s.Agent, s.ID)
		if err != nil || !ok {
			return gone()
		}
		if err := inboxChanged(s, rec, proven); err != nil {
			return err
		}
		ev := mark(&rec)
		ev.Agent, ev.Session, ev.Via = fs.Agent, fs.ID, via
		if err := store.Put(ctx, rec); err != nil {
			return err
		}
		if err := log.Append(ctx, &ev); err != nil {
			return err
		}
		d = Delivery{Session: fs, Via: via, Seq: ev.Seq}
		return nil
	})
	return d, err
}

// inboxChanged reports a fresh record that no longer names the inbox the
// channel was proven on.
func inboxChanged(s Session, rec session.Record, proven *session.Inbox) error {
	if proven == nil || session.SameInbox(rec.Inbox, proven) {
		return nil
	}
	return &RefusedError{RefuseChanged, fmt.Sprintf("%s's inbox changed while it was reached", s.ID)}
}

// threadIn checks that the daemon has the thread loaded in one of the
// given statuses.
func threadIn(ctx context.Context, c *codexsvc.Client, s Session, reason string, want ...string) error {
	st, err := c.ThreadStatus(ctx, s.ID)
	if err != nil {
		return err
	}
	switch {
	case slices.Contains(want, st):
		return nil
	case st == codexsvc.StatusNotLoaded:
		return &RefusedError{RefuseChanged, fmt.Sprintf("the Codex daemon no longer has %s loaded", s.ID)}
	}
	return &RefusedError{reason, fmt.Sprintf("the Codex daemon has %s %s", s.ID, st)}
}
