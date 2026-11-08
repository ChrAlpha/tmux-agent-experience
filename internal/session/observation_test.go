package session

import (
	"context"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

func TestContactIsIndependentOfStateAndMirrored(t *testing.T) {
	ctx := context.Background()
	store := Store{Dir: t.TempDir()}
	log := eventlog.Open(store.Dir)
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := Record{Agent: "codex", ID: "thread", Main: "working", Turn: "current", Since: since}
	first := since.Add(time.Hour)
	rec.Observe(first, "realm", true)
	rec, err := store.Save(ctx, log, rec, eventlog.Event{Event: "Start"})
	if err != nil {
		t.Fatal(err)
	}
	next, changed := Apply(rec, adapter.Transition{Action: adapter.Set, State: "idle", Turn: "old", Late: true}, first.Add(time.Minute))
	if changed {
		t.Fatal("late transition changed state")
	}
	next.Observe(first.Add(time.Minute), "realm", true)
	next, err = store.Save(ctx, log, next, eventlog.Event{Event: "Stop"})
	if err != nil {
		t.Fatal(err)
	}
	if next.State() != "working" || next.Turn != "current" || !next.Since.Equal(since) {
		t.Fatalf("state changed: %+v", next)
	}
	next.Observe(first, "realm", true)
	if !next.Observation.HookAt.Equal(first.Add(time.Minute)) {
		t.Fatal("contact moved backwards")
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	last := events[1]
	if last.Observation == nil || !last.Observation.HookAt.Equal(next.Observation.HookAt) || last.State != "working" || last.Since == nil || !last.Since.Equal(since) {
		t.Fatalf("incomplete observation event: %+v", last)
	}
}
func TestObservationAgeLabels(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		o         *Observation
		threshold time.Duration
		want      string
	}{
		{nil, time.Minute, "unknown"}, {&Observation{HookAt: now.Add(-time.Hour)}, time.Minute, "quiet"},
		{&Observation{HookAt: now}, time.Minute, "recent"}, {&Observation{HookAt: now.Add(2 * time.Minute)}, time.Minute, "unknown"},
		{nil, 0, ""},
	} {
		if got := ObservationLabel(tc.o, now, tc.threshold); got != tc.want {
			t.Fatalf("got %s want %s", got, tc.want)
		}
	}
}
