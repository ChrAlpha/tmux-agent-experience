package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// put stores the session's record as its hooks would leave it.
func put(t *testing.T, store session.Store, rec session.Record) {
	t.Helper()
	rec.Agent, rec.ID = "claude", "s1"
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

var s1 = Session{ID: "s1", Agent: "claude"}

// A prompt's turn counts once its hooks report it, not before: the state the
// session was in when the prompt was sent is no answer.
func TestWaitForTurnIgnoresStatesBeforeItStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := session.Store{Dir: t.TempDir()}
	put(t, store, session.Record{Main: "idle", Turn: "t0", Sent: time.Now()})
	turn := "t0"
	got := make(chan session.Record, 1)
	go func() {
		rec, err := Wait(ctx, store, s1, WaitOptions{States: []string{"idle", "blocked"}, Prompted: &turn, Grace: 5 * time.Second})
		if err != nil {
			t.Error(err)
		}
		got <- rec
	}()
	time.Sleep(3 * waitPoll)
	select {
	case rec := <-got:
		t.Fatalf("the state before the turn ended the wait: %+v", rec)
	default:
	}
	put(t, store, session.Record{Main: "working", Turn: "t1"})
	time.Sleep(3 * waitPoll)
	put(t, store, session.Record{Main: "idle", Turn: "t1"})
	if rec := <-got; rec.State() != "idle" || rec.Turn != "t1" {
		t.Fatalf("Wait = %+v", rec)
	}
}

// A turn that started and ended between two looks is seen by its traces:
// the prompt's mark is cleared and the turn is another.
func TestWaitSeesATurnThatEndedBetweenLooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := session.Store{Dir: t.TempDir()}
	put(t, store, session.Record{Main: "idle", Turn: "t1"})
	turn := "t0"
	rec, err := Wait(ctx, store, s1, WaitOptions{States: []string{"idle"}, Prompted: &turn, Grace: time.Second})
	if err != nil || rec.Turn != "t1" {
		t.Fatalf("Wait = %+v, %v", rec, err)
	}
}

func TestWaitForTurnReportsNoTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := session.Store{Dir: t.TempDir()}
	put(t, store, session.Record{Main: "idle", Turn: "t0", Sent: time.Now()})
	turn := "t0"
	rec, err := Wait(ctx, store, s1, WaitOptions{States: []string{"idle"}, Prompted: &turn, Grace: 300 * time.Millisecond})
	if !errors.Is(err, ErrNoTurn) || rec.State() != "idle" {
		t.Fatalf("Wait = %+v, %v; want ErrNoTurn", rec, err)
	}
}

func TestWaitEndsWithTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := session.Store{Dir: t.TempDir()}
	put(t, store, session.Record{Main: "working", Turn: "t1"})
	done := make(chan error, 1)
	go func() {
		_, err := Wait(ctx, store, s1, WaitOptions{States: []string{"idle"}})
		done <- err
	}()
	time.Sleep(2 * waitPoll)
	if err := store.Delete(ctx, "claude", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrEnded) {
		t.Fatalf("Wait err = %v, want ErrEnded", err)
	}
}

// A plain wait returns at once when the session is already in a state it
// waits for.
func TestWaitReturnsTheCurrentState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store := session.Store{Dir: t.TempDir()}
	put(t, store, session.Record{Main: "idle", Turn: "t1"})
	if rec, err := Wait(ctx, store, s1, WaitOptions{States: []string{"idle", "blocked"}}); err != nil || rec.State() != "idle" {
		t.Fatalf("Wait = %+v, %v", rec, err)
	}
}
