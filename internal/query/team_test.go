package query

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// scratchTmux keeps option lookups away from the developer's tmux server:
// no server runs in a fresh namespace, so every option is at its default.
func scratchTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
}

func agentCaller(agent, id string) Caller {
	return Caller{By: &eventlog.Actor{Agent: agent, Session: id}}
}

func working(agent, id string, owes ...eventlog.Obligation) session.Record {
	return session.Record{Agent: agent, ID: id, Main: adapter.Working, Turn: "t", Owes: owes}
}

func owedTo(agent, id, kind string, depth int, since time.Time) eventlog.Obligation {
	return eventlog.Obligation{ID: agent + "-" + id + "-" + kind, To: eventlog.Actor{Agent: agent, Session: id}, Kind: kind, Depth: depth, Turn: "t", Since: since}
}

func TestAnAskOpensAnObligationOneDeeperThanTheAskersOwn(t *testing.T) {
	scratchTmux(t)
	now := time.Now()
	// The person prompted a1; a1 asked b1; b1 now asks c1.
	v := View{records: []session.Record{
		working(proc.Claude, "a1"),
		working(proc.Codex, "b1", owedTo(proc.Claude, "a1", eventlog.OweReply, 1, now)),
		{Agent: proc.Pi, ID: "c1", Main: adapter.Idle},
	}}
	c1 := Session{Agent: proc.Pi, ID: "c1"}
	o, err := asking(context.Background(), v, agentCaller(proc.Codex, "b1"), c1, AskReply)
	if err != nil || o == nil || o.Depth != 2 || o.Kind != eventlog.OweReply || o.To != (eventlog.Actor{Agent: proc.Codex, Session: "b1"}) {
		t.Fatalf("ask from b1 = %+v %v", o, err)
	}
	o, err = asking(context.Background(), v, agentCaller(proc.Claude, "a1"), c1, AskFinal)
	if err != nil || o == nil || o.Depth != 1 || o.Kind != eventlog.OweFinal {
		t.Fatalf("ask from a1 = %+v %v", o, err)
	}
	// A person's prompt, or one from an agent tmag cannot answer back to,
	// opens none.
	for _, c := range []Caller{{}, {By: &eventlog.Actor{Agent: proc.Claude}}} {
		if o, err := asking(context.Background(), v, c, c1, AskFinal); o != nil || err != nil {
			t.Fatalf("ask from %+v = %+v %v", c.By, o, err)
		}
	}
}

func TestAnAskBeyondThePeerDepthIsRefused(t *testing.T) {
	scratchTmux(t)
	now := time.Now()
	v := View{records: []session.Record{
		working(proc.Claude, "c1"),
		working(proc.Codex, "d1", owedTo(proc.Claude, "c1", eventlog.OweReply, DefaultPeerDepth, now)),
		{Agent: proc.Pi, ID: "e1", Main: adapter.Idle},
	}}
	_, err := asking(context.Background(), v, agentCaller(proc.Codex, "d1"), Session{Agent: proc.Pi, ID: "e1"}, AskReply)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != RefuseDepth {
		t.Fatalf("an ask %d deep = %v", DefaultPeerDepth+1, err)
	}
}

func TestAWaitThatClosesACycleIsRefused(t *testing.T) {
	scratchTmux(t)
	now := time.Now()
	final := func(agent, id string) eventlog.Obligation { return owedTo(agent, id, eventlog.OweFinal, 1, now) }
	a, c := Session{Agent: proc.Claude, ID: "a1"}, Session{Agent: proc.Pi, ID: "c1"}
	// a1 waits for b1, which waits for c1: c1 waiting for a1 would close the
	// cycle, and so would b1 waiting for a1.
	v := View{records: []session.Record{
		working(proc.Claude, "a1"),
		working(proc.Codex, "b1", final(proc.Claude, "a1")),
		working(proc.Pi, "c1", final(proc.Codex, "b1")),
	}}
	for _, ask := range []struct {
		from   Caller
		target Session
	}{{agentCaller(proc.Pi, "c1"), a}, {agentCaller(proc.Codex, "b1"), a}} {
		_, err := asking(context.Background(), v, ask.from, ask.target, AskFinal)
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != RefuseCycle {
			t.Fatalf("%s waiting for %s = %v", ask.from.By.Session, ask.target.ID, err)
		}
		// An ask that does not hold the asker's turn closes no cycle.
		if _, err := asking(context.Background(), v, ask.from, ask.target, AskReply); err != nil {
			t.Fatalf("%s asking %s for a reply = %v", ask.from.By.Session, ask.target.ID, err)
		}
	}
	// Waiting the other way round is no cycle.
	if _, err := asking(context.Background(), v, agentCaller(proc.Claude, "a1"), c, AskFinal); err != nil {
		t.Fatalf("a1 waiting for c1 = %v", err)
	}
	// A waiter whose turn is over waits no more: b1 went idle after its wait
	// gave up.
	v.records[1].Main = adapter.Idle
	if _, err := asking(context.Background(), v, agentCaller(proc.Pi, "c1"), a, AskFinal); err != nil {
		t.Fatalf("c1 waiting for a1 past an idle b1 = %v", err)
	}
}

func TestAReplyClaimsOneReplyAskAtATime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, log := session.Store{Dir: dir}, eventlog.Open(dir)
	now := time.Now()
	a := Session{Agent: proc.Claude, ID: "a1"}
	replier := agentCaller(proc.Codex, "b1")
	ask := owedTo(proc.Claude, "a1", eventlog.OweReply, 1, now)
	b1 := working(proc.Codex, "b1", owedTo(proc.Pi, "c1", eventlog.OweReply, 1, now), ask)
	if err := store.Put(ctx, b1); err != nil {
		t.Fatal(err)
	}
	refused := func(err error, reason string) bool {
		var r *RefusedError
		return errors.As(err, &r) && r.Reason == reason
	}
	table, err := proc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w := &attach.World{Proc: table}
	o, err := claimReply(ctx, log, w, replier, a)
	if err != nil || o.ID != ask.ID || o.PID != os.Getpid() {
		t.Fatalf("claiming a1's ask = %+v %v", o, err)
	}
	// A second reply meanwhile finds the only ask claimed by a process that
	// runs: this one.
	if _, err := claimReply(ctx, log, w, replier, a); !refused(err, RefuseBusy) {
		t.Fatalf("a second claim = %v", err)
	}
	// A refused delivery releases the claim, and logs nothing.
	if err := releaseReply(ctx, log, replier, o, Delivery{}, &RefusedError{RefuseBusy, "busy"}); err != nil {
		t.Fatal(err)
	}
	if o, err = claimReply(ctx, log, w, replier, a); err != nil || o.ID != ask.ID {
		t.Fatalf("claiming again after a refusal = %+v %v", o, err)
	}
	// A claim whose process is gone is taken over.
	rec, _, _ := store.Get(ctx, proc.Codex, "b1")
	rec.Replying = []session.Claim{{ID: ask.ID, PID: 1 << 30}}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if o, err = claimReply(ctx, log, w, replier, a); err != nil || o.ID != ask.ID {
		t.Fatalf("taking over a dead claim = %+v %v", o, err)
	}
	// A pane write that failed may have reached the asker: it settles the
	// ask, after the reply's own budget.
	spent, cancel := context.WithCancel(ctx)
	cancel()
	if err := releaseReply(spent, log, replier, o, Delivery{Via: "pane"}, errors.New("typing failed")); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(ctx, proc.Codex, "b1")
	if len(rec.Owes) != 1 || rec.Owes[0].To.Session != "c1" || rec.Replying != nil {
		t.Fatalf("after the reply b1 owes %+v, claims %+v", rec.Owes, rec.Replying)
	}
	events, _ := log.Since(ctx, 0)
	if len(events) != 1 || events[0].Event != eventlog.Owes || events[0].By == nil || len(events[0].Owes) != 1 {
		t.Fatalf("settling logged %+v", events)
	}
	if _, err := claimReply(ctx, log, w, replier, a); !refused(err, RefuseUnasked) {
		t.Fatalf("a reply to nothing asked = %v", err)
	}
	rec.Owes = []eventlog.Obligation{owedTo(proc.Claude, "a1", eventlog.OweFinal, 1, now)}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := claimReply(ctx, log, w, replier, a); !refused(err, RefuseUnasked) {
		t.Fatalf("a reply to a final ask = %v", err)
	}
}

func TestASessionOwesNoMoreThanItMay(t *testing.T) {
	now := time.Now()
	rec := session.Record{Agent: proc.Claude, ID: "b1", Main: adapter.Idle}
	for range session.MaxOwes - 1 {
		rec.Owe(eventlog.Actor{Agent: proc.Codex, Session: "a1"}, eventlog.OweReply, 1, now)
	}
	gate := roomy(liveGate)
	if err := gate(rec, now); err != nil {
		t.Fatalf("room for one more: %v", err)
	}
	rec.Owe(eventlog.Actor{Agent: proc.Codex, Session: "a1"}, eventlog.OweReply, 1, now)
	var refused *RefusedError
	if err := gate(rec, now); !errors.As(err, &refused) || refused.Reason != RefuseOwing {
		t.Fatalf("an ask beyond %d = %v", session.MaxOwes, err)
	}
}

func TestAnEndedAskerAddsNoDepth(t *testing.T) {
	scratchTmux(t)
	now := time.Now()
	v := View{records: []session.Record{
		working(proc.Codex, "b1", owedTo(proc.Claude, "gone", eventlog.OweReply, DefaultPeerDepth, now)),
		{Agent: proc.Pi, ID: "c1", Main: adapter.Idle},
	}}
	o, err := asking(context.Background(), v, agentCaller(proc.Codex, "b1"), Session{Agent: proc.Pi, ID: "c1"}, AskReply)
	if err != nil || o == nil || o.Depth != 1 {
		t.Fatalf("an ask by an agent whose asker ended = %+v %v", o, err)
	}
}

func TestAReplyMeetsItsObligationUnlessProvenUndelivered(t *testing.T) {
	logged := Delivery{Seq: 7, Via: "inbox"}
	for _, c := range []struct {
		name string
		d    Delivery
		err  error
		want bool
	}{
		{"delivered", logged, nil, true},
		{"asker ended", Delivery{}, sessionEnded("a1"), true},
		{"unknown after it was logged", logged, errors.New("connection reset"), true},
		{"typed, then not logged", Delivery{Via: "pane"}, errors.New("disk full"), true},
		{"failed before any write", Delivery{}, errors.New("disk full"), false},
		{"refused", Delivery{}, &RefusedError{RefuseBusy, "busy"}, false},
		{"not taken", logged, &NotTakenError{Seq: 7, Target: "a1", Err: errors.New("refused")}, false},
	} {
		if got := answered(c.d, c.err); got != c.want {
			t.Errorf("%s: answered = %v", c.name, got)
		}
	}
}

func TestWaitManyReportsEachAnswerAndEnd(t *testing.T) {
	ctx := context.Background()
	store := session.Store{Dir: t.TempDir()}
	now := time.Now()
	ask := owedTo(proc.Claude, "a1", eventlog.OweReply, 1, now)
	b1 := working(proc.Codex, "b1", ask)
	c1 := working(proc.Pi, "c1", owedTo(proc.Claude, "a1", eventlog.OweReply, 1, now))
	for _, r := range []session.Record{b1, c1} {
		if err := store.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	awaited := []Awaited{
		{Session: Session{Agent: proc.Codex, ID: "b1"}, Owed: []string{ask.ID}},
		{Session: Session{Agent: proc.Pi, ID: "c1"}, Owed: []string{c1.Owes[0].ID}},
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if reached, err := WaitMany(short, store, awaited, false); !errors.Is(err, context.DeadlineExceeded) || len(reached) != 0 {
		t.Fatalf("nothing settled yet: %+v %v", reached, err)
	}
	b1.Settle(ask.ID)
	if err := store.Put(ctx, b1); err != nil {
		t.Fatal(err)
	}
	reached, err := WaitMany(ctx, store, awaited, false)
	if err != nil || len(reached) != 1 || !reached[0].Settled || reached[0].Session.ID != "b1" {
		t.Fatalf("any, after b1 replied = %+v %v", reached, err)
	}
	if err := store.Delete(ctx, proc.Pi, "c1"); err != nil {
		t.Fatal(err)
	}
	reached, err = WaitMany(ctx, store, awaited, true)
	if err != nil || len(reached) != 2 || !reached[1].Ended {
		t.Fatalf("all, after c1 ended = %+v %v", reached, err)
	}
}

func TestANoticeIsToldOnceUnlessItWasNotTaken(t *testing.T) {
	now := time.Now()
	rec := session.Record{Agent: proc.Claude, ID: "a1", Main: adapter.Idle}
	gate := untold(42, liveGate)
	if err := gate(rec, now); err != nil {
		t.Fatalf("a first notice: %v", err)
	}
	prior := rec
	prompt{notice: 42}.mark(&rec)
	if err := gate(rec, now); !errors.Is(err, ErrTold) {
		t.Fatalf("a second notice of the same end: %v", err)
	}
	if err := untold(43, liveGate)(rec, now); err != nil {
		t.Fatalf("a notice of another end: %v", err)
	}
	// A delivery proven not taken takes the telling back.
	ctx := context.Background()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	s := Session{Agent: rec.Agent, ID: rec.ID}
	if err := unmark(eventlog.Open(store.Dir), store, s, prior, rec)(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get(ctx, rec.Agent, rec.ID)
	if err := gate(got, now); err != nil {
		t.Fatalf("a notice after one not taken: %v (%v)", err, got.Notified)
	}
}

func TestAnEndIsToldOnlyWithinItsWindow(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	asker := eventlog.Actor{Agent: proc.Claude, Session: "a1"}
	owed := []eventlog.Obligation{
		{ID: "o1", To: asker, Kind: eventlog.OweReply},
		{ID: "o2", To: asker, Kind: eventlog.OweReply},
		{ID: "o3", To: eventlog.Actor{Agent: proc.Pi, Session: "c1"}, Kind: eventlog.OweFinal},
	}
	old := eventlog.Event{Event: eventlog.Exit, Agent: proc.Codex, Session: "b0", Owes: owed, Time: time.Now().Add(-session.EndWindow)}
	recent := eventlog.Event{Event: eventlog.End, Agent: proc.Codex, Session: "b1", Owes: owed}
	for _, ev := range []*eventlog.Event{&old, &recent} {
		if err := log.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	ended, askers, until, err := Ending(ctx, log, recent.Seq)
	if err != nil || ended.Session != "b1" || len(askers) != 1 || askers[0] != asker || !until.Equal(recent.Time.Add(session.EndWindow)) {
		t.Fatalf("Ending(recent) = %v %v %v %v", ended, askers, until, err)
	}
	if ended, _, until, err := Ending(ctx, log, old.Seq); err != nil || ended.Session != "b0" || time.Now().Before(until) {
		t.Fatalf("Ending(old) = %v %v %v: its window should have closed", ended, until, err)
	}
}

func TestNoticesAreRememberedWithinTheirWindowAndBounded(t *testing.T) {
	now := time.Now()
	var rec session.Record
	gate := func(seq int64, at time.Time) error { return untold(seq, liveGate)(rec, at) }
	for seq := range int64(session.MaxNotices) {
		if err := gate(seq+1, now); err != nil {
			t.Fatalf("notice %d: %v", seq+1, err)
		}
		rec.Notify(seq+1, now)
	}
	// Full: a new end waits, and none it remembers is forgotten for it.
	var refused *RefusedError
	if err := gate(1000, now); !errors.As(err, &refused) || refused.Reason != RefuseBusy {
		t.Fatalf("a notice beyond %d: %v", session.MaxNotices, err)
	}
	if err := gate(1, now); !errors.Is(err, ErrTold) {
		t.Fatalf("the first end, told: %v", err)
	}
	// Past the window it is forgotten, at the next write as at a hook.
	later := now.Add(session.NoticeWindow)
	if err := gate(1, later); err != nil {
		t.Fatalf("an end told before the window: %v", err)
	}
	rec.Notify(1000, later)
	if len(rec.Notified) != 1 {
		t.Fatalf("a write after the window keeps %d notices", len(rec.Notified))
	}
}

func TestADeliveryThatCannotBeLoggedLeavesNoMark(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := session.Store{Dir: dir}
	rec := session.Record{Agent: proc.Pi, ID: "a1", Main: adapter.Working}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// The log cannot be written: its file is a directory.
	log := eventlog.Open(dir)
	if err := os.Mkdir(filepath.Join(dir, "events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := prompt{text: "[tmag] Notice", ask: Notice, notice: 42}
	_, _, _, err := recordDelivery(ctx, log, Session{Agent: rec.Agent, ID: rec.ID}, untold(42, invitedGate), "inbox", nil,
		func() error { return sessionEnded(rec.ID) },
		func(r *session.Record) eventlog.Event { p.mark(r); r.Queued = time.Now(); return sentEvent(*r, p) }, nil)
	if err == nil {
		t.Fatal("a delivery that could not be logged went on")
	}
	got, _, _ := store.Get(ctx, rec.Agent, rec.ID)
	if got.Notified != nil || !got.Queued.IsZero() {
		t.Fatalf("an unlogged delivery left notified %v, queued %v", got.Notified, got.Queued)
	}
	if err := untold(42, invitedGate)(got, time.Now()); err != nil {
		t.Fatalf("the notice after its log recovers: %v", err)
	}
}
