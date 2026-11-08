package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

func TestWaitForTurnIgnoresStatesBeforeItStarts(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	ctx := context.Background()
	appendEvent := func(event, state string) int64 {
		e := eventlog.Event{Agent: "claude", Session: "s1", Event: event, State: state}
		if err := log.Append(ctx, &e); err != nil {
			t.Fatal(err)
		}
		return e.Seq
	}
	send := appendEvent("Send", "")
	// A late idle notification lands between delivery and the turn's start.
	appendEvent("Notification", "idle")
	appendEvent("UserPromptSubmit", "working")
	stop := appendEvent("Stop", "idle")

	o := WaitOptions{States: []string{"idle", "blocked"}, After: send, StartGrace: time.Second}
	ev, err := Wait(ctx, log, Session{ID: "s1", Agent: "claude"}, o)
	if err != nil || ev.Seq != stop {
		t.Fatalf("Wait = %+v, %v; want the Stop event %d", ev, err, stop)
	}
}

func TestWaitForTurnReportsNoTurn(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	ctx := context.Background()
	e := eventlog.Event{Agent: "claude", Session: "s1", Event: "Send"}
	if err := log.Append(ctx, &e); err != nil {
		t.Fatal(err)
	}
	idle := eventlog.Event{Agent: "claude", Session: "s1", Event: "Notification", State: "idle"}
	if err := log.Append(ctx, &idle); err != nil {
		t.Fatal(err)
	}
	o := WaitOptions{States: []string{"idle"}, After: e.Seq, StartGrace: 300 * time.Millisecond}
	if _, err := Wait(ctx, log, Session{ID: "s1", Agent: "claude"}, o); !errors.Is(err, ErrNoTurn) {
		t.Fatalf("Wait err = %v, want ErrNoTurn", err)
	}
}

func TestWaitEndsWithTheSession(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	ctx := context.Background()
	for _, e := range []eventlog.Event{
		{Agent: "codex", Session: "s1", Event: eventlog.Detach},
		{Agent: "claude", Session: "s1", Event: "Stop", State: "idle"}, // another agent's session
		{Agent: "codex", Session: "s1", Event: eventlog.Exit},
	} {
		if err := log.Append(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	o := WaitOptions{States: []string{"idle"}, After: 0}
	if _, err := Wait(ctx, log, Session{ID: "s1", Agent: "codex"}, o); !errors.Is(err, ErrEnded) {
		t.Fatalf("Wait err = %v, want ErrEnded", err)
	}
}

func TestHistoricalWaitRejectsLossEvenWhenCurrentStateMatches(t *testing.T) {
	for _, grace := range []time.Duration{0, time.Nanosecond} {
		log := eventlog.Open(t.TempDir())
		log.MaxBytes = 1
		sent := eventlog.Event{Agent: "claude", Session: "s1", Event: eventlog.Send}
		if err := log.Append(context.Background(), &sent); err != nil {
			t.Fatal(err)
		}
		for range 4 {
			if err := log.Append(context.Background(), &eventlog.Event{Agent: "claude", Session: "s1", Event: "Stop", State: "idle"}); err != nil {
				t.Fatal(err)
			}
		}
		ev, err := Wait(context.Background(), log, Session{Agent: "claude", ID: "s1", State: "idle"}, WaitOptions{
			After: sent.Seq, States: []string{"idle"}, StartGrace: grace,
		})
		if !errors.Is(err, eventlog.ErrHistoryUnavailable) || ev.State != "" {
			t.Fatalf("grace %s invented a historical result: %+v, %v", grace, ev, err)
		}
	}
}

func TestWaitAcceptsRetainedNumericCursorWithoutNewIdentity(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	sent := eventlog.Event{Agent: "claude", Session: "s1", Event: eventlog.Send}
	if err := log.Append(context.Background(), &sent); err != nil {
		t.Fatal(err)
	}
	stopped := eventlog.Event{Agent: "claude", Session: "s1", Event: "Stop", State: "idle"}
	if err := log.Append(context.Background(), &stopped); err != nil {
		t.Fatal(err)
	}
	e, err := Wait(context.Background(), log, Session{Agent: "claude", ID: "s1"}, WaitOptions{After: sent.Seq, States: []string{"idle"}})
	if err != nil || e.Seq != stopped.Seq {
		t.Fatalf("numeric wait = %+v, %v", e, err)
	}
}
