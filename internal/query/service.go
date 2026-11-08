package query

import (
	"context"
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
	if host.PID == 0 || !proc.Running(host.PID, host.Start) {
		return nil, &RefusedError{RefuseChanged, fmt.Sprintf("the service %s ran in has exited", s.ID)}
	}
	sock := codexsvc.Socket()
	c, err := codexsvc.Dial(ctx, sock)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Codex daemon at %s: %w", sock, err)
	}
	if t, err := proc.Snapshot(); err == nil && slices.Contains(t.Ancestors(host.PID), c.PeerPID) {
		return c, nil
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
	rec, ok, err := store.Get(s.Agent, s.ID)
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
	err = store.Lock(ctx, s.Agent, s.ID, func() error {
		fresh, _, err := Load(ctx, store)
		if err != nil {
			return err
		}
		fs, ok := fresh.Session(s.Agent, s.ID)
		if !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
		}
		if err := verb.gate(fs, fresh, time.Now()); err != nil {
			return err
		}
		rec, ok, err := store.Current(ctx, log, s.Agent, s.ID)
		if err != nil || !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s has ended", s.ID)}
		}
		ev := verb.mark(&rec)
		if err := store.Put(rec); err != nil {
			return err
		}
		ev.Agent, ev.Session, ev.Via = fs.Agent, fs.ID, driver.Service
		if err := log.Append(ctx, &ev); err != nil {
			return err
		}
		d = Delivery{Session: fs, Via: driver.Service, Seq: ev.Seq, Cursor: ev.Cursor()}
		return nil
	})
	if err != nil {
		return Delivery{}, err
	}
	ctx, cancelDelivery := context.WithTimeout(outer, serviceTimeout)
	defer cancelDelivery()
	if err := verb.deliver(ctx, c); err != nil {
		return d, fmt.Errorf("logged at seq %d, but the Codex daemon did not take it: %w", d.Seq, err)
	}
	return d, nil
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
