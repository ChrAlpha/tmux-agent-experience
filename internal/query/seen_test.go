package query

import (
	"context"
	"os"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// A person marks a finished turn seen once, under the session's lock and
// with an event; a turn the session is not idle on is refused.
func TestMarkSeenDecidesAgainUnderTheLock(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	rec := session.Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Turn: "t1"}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, seq, changed, err := markSeen(ctx, log, rec.Agent, rec.ID, "t1", Caller{})
	if err != nil || !changed || seq == 0 || got.Seen != "t1" {
		t.Fatalf("markSeen = %+v, %d, %v, %v", got, seq, changed, err)
	}
	if _, _, changed, err := markSeen(ctx, log, rec.Agent, rec.ID, "t1", Caller{}); err != nil || changed {
		t.Fatalf("marking a seen turn again = %v, %v", changed, err)
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != eventlog.Seen || events[0].Turn != "t1" {
		t.Fatalf("events = %+v", events)
	}

	// Already seen is quiet only while the session is still idle on that
	// turn; a working next turn is busy, and a finished one is changed.
	working := got
	working.Main, working.Turn = adapter.Working, "t2"
	if err := store.Put(ctx, working); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := markSeen(ctx, log, working.Agent, working.ID, "t1", Caller{}); !isRefused(err, RefuseBusy) {
		t.Fatalf("a seen turn while the next works: %v", err)
	}
	finished := working
	finished.Main = adapter.Idle
	if err := store.Put(ctx, finished); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := markSeen(ctx, log, finished.Agent, finished.ID, "t1", Caller{}); !isRefused(err, RefuseChanged) {
		t.Fatalf("a seen turn after the next finished: %v", err)
	}

	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := markSeen(ctx, log, rec.Agent, rec.ID, "t2", Caller{}); !isRefused(err, RefuseChanged) {
		t.Fatalf("a turn the session is not idle on: %v", err)
	}
	rec.Main, rec.Turn = adapter.Working, "t1"
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := markSeen(ctx, log, rec.Agent, rec.ID, "t1", Caller{}); !isRefused(err, RefuseBusy) {
		t.Fatalf("a working session: %v", err)
	}
}

// An agent never marks a turn seen through an automatic path either: the
// caller is refused before a candidate or a tmux observation is considered.
func TestMarkShownRefusesAnAgent(t *testing.T) {
	w := &attach.World{Proc: proc.NewTable([]proc.Process{
		{PID: 1, Comm: "init"},
		{PID: 10, PPID: 1, Comm: "claude", Args: "claude", Start: "c"},
		{PID: os.Getpid(), PPID: 10, Comm: "tmag"},
	})}
	v := Build(w, []session.Record{{
		Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Turn: "t1",
		Host: session.Host{PID: 10, Start: "c", Role: proc.Terminal},
	}})
	if _, err := MarkShown(context.Background(), eventlog.Open(t.TempDir()), v, w, ""); !isRefused(err, RefuseAgent) {
		t.Fatalf("an agent's automatic mark: %v", err)
	}
}

// A turn already seen is a quiet success: no event, no error.
func TestAlreadySeenIsQuiet(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	rec := session.Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Turn: "t1", Seen: "t1"}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, seq, changed, err := markSeen(ctx, log, rec.Agent, rec.ID, "t1", Caller{})
	if err != nil || changed || seq != 0 || got.Seen != "t1" {
		t.Fatalf("markSeen = %+v, %d, %v, %v", got, seq, changed, err)
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

// An agent never marks a turn seen, its own or another's.
func TestAgentsNeverMarkTurnsSeen(t *testing.T) {
	if err := seenGate(Caller{}); err != nil {
		t.Fatalf("a person was refused: %v", err)
	}
	for _, by := range []string{proc.Claude, proc.Codex, proc.Pi, proc.OMP} {
		if err := seenGate(Caller{By: &eventlog.Actor{Agent: by}}); !isRefused(err, RefuseAgent) {
			t.Errorf("an %s agent: %v", by, err)
		}
	}
}
