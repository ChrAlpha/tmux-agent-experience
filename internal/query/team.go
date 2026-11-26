package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Agents' relations are flat: any agent may ask any other, and what a tree
// of parents and children would give is kept per ask. An ask an agent makes
// with send --wait or send --reply opens an obligation on the receiver,
// which the answer closes; tmag refuses a wait that would close a cycle of
// waits, and an ask that passes work on further than @tmag-peer-depth. Who
// started a session, and how each is tied to the caller, are legible to it
// without making either the other's parent.

// PeerDepthOption is the tmux option that bounds how many times agents pass
// work on: a person's prompt starts at 0, and each ask an agent makes while
// it owes an answer adds one. "off" bounds it only by maxPeerDepth.
const PeerDepthOption = "@tmag-peer-depth"

// DefaultPeerDepth is the bound where PeerDepthOption does not set one, and
// maxPeerDepth the deepest an ask may be, which bounds an obligation's depth.
const (
	DefaultPeerDepth = 3
	maxPeerDepth     = 255
)

// peerDepth is the bound across the tmux servers tmag finds: the least any of
// them allows, a server that sets none, or no number, allowing
// DefaultPeerDepth.
func peerDepth(ctx context.Context) int {
	quick, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	limit := 0
	servers, _ := tmux.Servers(quick)
	for _, srv := range servers {
		value, err := srv.Option(quick, PeerDepthOption)
		if err != nil {
			continue // no server listens on this socket
		}
		value = strings.TrimSpace(value)
		n, err := strconv.Atoi(value)
		switch {
		case value == "off":
			n = maxPeerDepth
		case err != nil || n < 1 || n > maxPeerDepth:
			n = DefaultPeerDepth
		}
		if limit == 0 || n < limit {
			limit = n
		}
	}
	if limit == 0 {
		return DefaultPeerDepth
	}
	return limit
}

// Ties is how a session relates to the agent session that runs a command,
// from what tmag keeps of each relation; none makes one the other's parent.
type Ties struct {
	Own     bool // it is the caller's own session
	Started bool // the caller started it
	OwesYou bool // it owes the caller an answer
	YouOwe  bool // the caller owes it an answer
}

// Any reports whether s is tied to the caller at all.
func (t Ties) Any() bool { return t.Own || t.Started || t.OwesYou || t.YouOwe }

// TiesOf is how s relates to the caller c, as the sessions of v report it; a
// caller tmag proves no session of has none.
func (v View) TiesOf(c Caller, s Session) Ties {
	if c.By == nil || c.By.Session == "" {
		return Ties{}
	}
	me := asker(c)
	t := Ties{Own: c.Is(s), Started: s.StartedBy != nil && *s.StartedBy == me}
	t.OwesYou = slices.ContainsFunc(s.Owes, func(o eventlog.Obligation) bool { return o.To == me })
	if mine, ok := v.Session(me.Agent, me.Session); ok {
		t.YouOwe = slices.ContainsFunc(mine.Owes, func(o eventlog.Obligation) bool { return o.To == actorOf(s) })
	}
	return t
}

// Mine is what of v is tied to the caller c: its own session, the sessions
// it started, owes an answer or is owed one by, and the panes that show them.
func (v View) Mine(c Caller) View {
	out := v
	out.Sessions, out.Panes = []Session{}, []Pane{}
	kept := map[[2]string]bool{}
	for _, s := range v.Sessions {
		if v.TiesOf(c, s).Any() {
			out.Sessions = append(out.Sessions, s)
			kept[[2]string{s.Agent, s.ID}] = true
		}
	}
	for _, p := range v.Panes {
		if p.Session != "" && kept[[2]string{p.Agent, p.Session}] {
			out.Panes = append(out.Panes, p)
		}
	}
	return out
}

// asker is the session that asks, as an obligation names it.
func asker(c Caller) eventlog.Actor { return eventlog.Actor{Agent: c.By.Agent, Session: c.By.Session} }

func actorOf(s Session) eventlog.Actor { return eventlog.Actor{Agent: s.Agent, Session: s.ID} }

// asking checks an agent's ask of s against the team's bounds and returns
// the obligation it opens on s, or nil for a prompt that opens none: a
// person's, or one from an agent tmag cannot answer back to.
func asking(ctx context.Context, v View, c Caller, s Session, ask Ask) (*eventlog.Obligation, error) {
	o, err := deepened(ctx, v, c)
	if o == nil || err != nil {
		return nil, err
	}
	if ask == AskFinal {
		o.Kind = eventlog.OweFinal
		if waitsFor(v, actorOf(s), o.To, time.Now()) {
			return nil, &RefusedError{RefuseCycle, fmt.Sprintf("%s waits for your answer, itself or through others; waiting for its answer in turn would hold you both until a timeout", s.ID)}
		}
	}
	return o, nil
}

// deepened is the reply obligation an agent's ask opens, one deeper than
// the deepest it owes, unless that passes the work on further than
// @tmag-peer-depth allows; nil for a caller that is owed nothing: a person,
// or an agent tmag cannot answer back to.
func deepened(ctx context.Context, v View, c Caller) (*eventlog.Obligation, error) {
	if c.By == nil || c.By.Session == "" {
		return nil, nil
	}
	from := asker(c)
	depth := 1
	if rec, ok := v.record(from.Agent, from.Session); ok {
		for _, o := range rec.Owing(time.Now()) {
			if _, runs := v.record(o.To.Agent, o.To.Session); runs {
				depth = max(depth, o.Depth+1)
			}
		}
	}
	depth = min(depth, maxPeerDepth+1)
	if limit := peerDepth(ctx); depth > limit {
		return nil, &RefusedError{RefuseDepth, fmt.Sprintf("you were asked for work an agent was asked for in turn; your ask would pass it on %d times, and %s allows %d", depth, PeerDepthOption, limit)}
	}
	return &eventlog.Obligation{To: from, Kind: eventlog.OweReply, Depth: depth}, nil
}

// waitsFor reports whether from waits for to's answer, itself or through
// others: a session waits for another that owes it a final answer while its
// own turn, which runs the wait, is working.
func waitsFor(v View, from, to eventlog.Actor, now time.Time) bool {
	seen := map[eventlog.Actor]bool{from: true}
	for queue := []eventlog.Actor{from}; len(queue) > 0; queue = queue[1:] {
		x := queue[0]
		if r, ok := v.record(x.Agent, x.Session); !ok || r.State() != adapter.Working {
			continue
		}
		for _, r := range v.records {
			y := eventlog.Actor{Agent: r.Agent, Session: r.ID}
			if seen[y] || !owesWait(r, x, now) {
				continue
			}
			if y == to {
				return true
			}
			seen[y] = true
			queue = append(queue, y)
		}
	}
	return false
}

// roomy admits an ask only to a session that may owe one more answer, after
// gate.
func roomy(gate stateGate) stateGate {
	return func(rec session.Record, now time.Time) error {
		if len(rec.Owing(now)) >= session.MaxOwes {
			return &RefusedError{RefuseOwing, fmt.Sprintf("%s owes %d answers already", rec.ID, session.MaxOwes)}
		}
		return gate(rec, now)
	}
}

// owesWait reports whether r owes x a final answer x may still wait for.
func owesWait(r session.Record, x eventlog.Actor, now time.Time) bool {
	for _, o := range r.Owes {
		if o.To == x && session.Waiting(o, now) {
			return true
		}
	}
	return false
}

// claimReply claims for this process, under the caller's lock, the
// obligation a reply from c to s settles: the oldest ask of s for a reply
// that no other running reply of c's is being delivered for. A final ask is
// answered in the final message.
func claimReply(ctx context.Context, log *eventlog.Log, w *attach.World, c Caller, s Session) (session.Claim, error) {
	me := session.Claim{PID: os.Getpid()}
	if p, ok := w.Proc.Get(me.PID); ok {
		me.Start = p.Start
	}
	alive := func(o session.Claim) bool { return proc.Running(ctx, o.PID, o.Start) }
	store := session.Store{Dir: log.Dir}
	err := store.Lock(ctx, c.By.Agent, c.By.Session, func() error {
		rec, _, err := store.Get(ctx, c.By.Agent, c.By.Session)
		if err != nil {
			return err
		}
		final, claimed := false, false
		for _, o := range rec.Owing(time.Now()) {
			switch {
			case o.To != actorOf(s):
			case o.Kind == eventlog.OweFinal:
				final = true
			case rec.Claimed(o.ID, alive):
				claimed = true
			default:
				me.ID = o.ID
				rec.Claim(me)
				return store.Put(ctx, rec)
			}
		}
		switch {
		case claimed:
			return &RefusedError{RefuseBusy, fmt.Sprintf("a reply of yours to %s is on its way", s.ID)}
		case final:
			return &RefusedError{RefuseUnasked, fmt.Sprintf("%s asked for your answer in your final message, which it reads when your turn ends", s.ID)}
		}
		return &RefusedError{RefuseUnasked, fmt.Sprintf("%s asked nothing of you that awaits a reply", s.ID)}
	})
	return me, err
}

// cleanupTimeout bounds what a verb settles after its delivery, which runs
// on even when the verb's own budget ran out.
const cleanupTimeout = 2 * time.Second

// releaseReply ends this process's claim: its obligation settled, with the
// Owes event that reports it, when the reply was delivered, may have been, or
// its asker ended; the claim released otherwise, so that the reply may run
// again.
func releaseReply(outer context.Context, log *eventlog.Log, c Caller, claim session.Claim, d Delivery, delivery error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(outer), cleanupTimeout)
	defer cancel()
	store := session.Store{Dir: log.Dir}
	return store.Lock(ctx, c.By.Agent, c.By.Session, func() error {
		rec, ok, err := store.Get(ctx, c.By.Agent, c.By.Session)
		if err != nil || !ok {
			return err
		}
		if !answered(d, delivery) {
			if rec.Unclaim(claim) {
				return store.Put(ctx, rec)
			}
			return nil
		}
		if !rec.Settle(claim.ID) {
			return nil
		}
		ev := eventlog.Event{Event: eventlog.Owes}
		c.stamp(&ev)
		return store.Report(ctx, log, rec, &ev)
	})
}

// answered reports whether a reply's delivery meets its obligation: it was
// delivered, or may have been, since a channel began writing it, or its
// asker has ended.
func answered(d Delivery, err error) bool {
	switch {
	case err == nil, errors.Is(err, ErrEnded):
		return true
	case errors.As(err, new(*RefusedError)), errors.As(err, new(*NotTakenError)):
		return false
	}
	return d.Via != ""
}

// forgetAsker closes what the caller owes to the asker target named, when
// that asker no longer runs: a reply to it has nowhere to go. err, why the
// target did not resolve, is returned with what closing met.
func forgetAsker(ctx context.Context, log *eventlog.Log, v View, c Caller, target string, err error) error {
	if c.By == nil || c.By.Session == "" || !errors.Is(err, ErrNotFound) || len(target) < 4 {
		return err
	}
	store := session.Store{Dir: log.Dir}
	return errors.Join(err, store.Lock(ctx, c.By.Agent, c.By.Session, func() error {
		rec, ok, err := store.Get(ctx, c.By.Agent, c.By.Session)
		if err != nil || !ok {
			return err
		}
		closed := false
		for _, o := range slices.Clone(rec.Owes) {
			if _, runs := v.record(o.To.Agent, o.To.Session); !runs && strings.HasPrefix(o.To.Session, target) {
				closed = rec.Settle(o.ID) || closed
			}
		}
		if !closed {
			return nil
		}
		ev := eventlog.Event{Event: eventlog.Owes}
		c.stamp(&ev)
		return store.Report(ctx, log, rec, &ev)
	}))
}

// Ending reads the end tmag logged at seq: the session that ended, the
// agents it owed a reply, and until when, session.EndWindow after the end,
// tmag tells them that none will come. Only an end tmag logged names
// askers, so nobody else can have tmag tell an agent so; Tell tells each one
// once.
func Ending(ctx context.Context, log *eventlog.Log, seq int64) (ended eventlog.Actor, askers []eventlog.Actor, until time.Time, err error) {
	events, err := log.Since(ctx, seq-1)
	if err != nil {
		return ended, nil, until, err
	}
	if len(events) == 0 || events[0].Seq != seq || !events[0].Ended() {
		return ended, nil, until, fmt.Errorf("%w: no session ended at seq %d", ErrNotFound, seq)
	}
	end := events[0]
	until = end.Time.Add(session.EndWindow)
	ended = eventlog.Actor{Agent: end.Agent, Session: end.Session}
	for _, o := range end.Owes {
		if o.Kind == eventlog.OweReply && !slices.Contains(askers, o.To) {
			askers = append(askers, o.To)
		}
	}
	return ended, askers, until, nil
}

// ErrTold reports an asker tmag already told of an end.
var ErrTold = errors.New("already told of that end")

// untold admits a notice of the end at seq only to a session tmag has not
// told of it, and that has room to remember it, after gate: the check runs
// under the receiver's lock, as the mark that records the telling does.
func untold(seq int64, gate stateGate) stateGate {
	return func(rec session.Record, now time.Time) error {
		told := rec.Notices(now)
		switch {
		case slices.ContainsFunc(told, func(t session.Told) bool { return t.Seq == seq }):
			return ErrTold
		case len(told) >= session.MaxNotices:
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s was told of %d ends in the last %s", rec.ID, len(told), session.NoticeWindow)}
		}
		return gate(rec, now)
	}
}

// Forget closes what other sessions owe ended, which no answer reaches any
// more, each under its own lock with the Owes event that reports it.
func Forget(ctx context.Context, log *eventlog.Log, ended eventlog.Actor) error {
	store := session.Store{Dir: log.Dir}
	recs, _, err := store.All(ctx)
	if err != nil {
		return err
	}
	owesIt := func(r session.Record) bool {
		return slices.ContainsFunc(r.Owes, func(o eventlog.Obligation) bool { return o.To == ended })
	}
	var errs []error
	for _, r := range recs {
		if !owesIt(r) {
			continue
		}
		errs = append(errs, store.Lock(ctx, r.Agent, r.ID, func() error {
			cur, ok, err := store.Get(ctx, r.Agent, r.ID)
			if err != nil || !ok || !owesIt(cur) {
				return err
			}
			for _, o := range slices.Clone(cur.Owes) {
				if o.To == ended {
					cur.Settle(o.ID)
				}
			}
			return store.Report(ctx, log, cur, &eventlog.Event{Event: eventlog.Owes})
		}))
	}
	return errors.Join(errs...)
}

// Tell tells asker that ended, which it asked for a reply, ended without
// giving one: tmag's own notice, about the end logged at seq. It goes
// through the asker's own channel in any state, or its pane while it is
// idle.
func Tell(ctx context.Context, log *eventlog.Log, asker, ended eventlog.Actor, seq int64) (Delivery, error) {
	text := fmt.Sprintf("%s Notice: %s %s ended without answering your prompt. No answer needed.", Marker, ended.Agent, ended.Session)
	return deliver(ctx, log, asker.Session, prompt{text: text, ask: Notice, notice: seq}, "tell", "tell")
}
