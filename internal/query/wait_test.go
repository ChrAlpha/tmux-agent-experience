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
	ev, err := Wait(ctx, log, Agent{ID: "s1"}, o)
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
	if _, err := Wait(ctx, log, Agent{ID: "s1"}, o); !errors.Is(err, ErrNoTurn) {
		t.Fatalf("Wait err = %v, want ErrNoTurn", err)
	}
}
