package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func snapshotWorld() *attach.World { return &attach.World{Proc: proc.NewTable(nil)} }

func snapshotView(t *testing.T, store session.Store) (View, *attach.World, error) {
	t.Helper()
	recs, err := store.All()
	w := snapshotWorld()
	return Build(w, recs), w, err
}

func saveSnapshotRecord(t *testing.T, ctx context.Context, store session.Store, log *eventlog.Log, rec session.Record) {
	t.Helper()
	err := store.Lock(ctx, rec.Agent, rec.ID, func() error {
		_, err := store.Save(ctx, log, rec, "Changed")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func snapshotPulse(t *testing.T, ctx context.Context, log *eventlog.Log, count int) {
	t.Helper()
	for range count {
		if err := log.Append(ctx, &eventlog.Event{Event: "Pulse"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotEmptyStartupCopiesFirstTimestampEvent(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	since := time.Unix(100, 123456789).UTC()
	rec := session.Record{Agent: "claude", ID: "first", Main: "working", Since: since}
	snap, err := takeSnapshot(ctx, log, func(ctx context.Context) (View, *attach.World, error) {
		// The first event arrives after the empty starting cursor, while the
		// state scan itself still reports an empty image.
		v := Build(snapshotWorld(), nil)
		saveSnapshotRecord(t, ctx, store, log, rec)
		return v, snapshotWorld(), nil
	})
	if err != nil || snap.Seq != 0 || len(snap.Backlog.Events) != 1 {
		t.Fatalf("first capture = %+v, %v", snap, err)
	}
	e := snap.Backlog.Events[0]
	if e.Seq < time.Now().Add(-time.Hour).UnixMicro() || e.Since == nil || !e.Since.Equal(since) {
		t.Fatalf("first timestamp event = %+v", e)
	}
	if b, err := log.Replay(ctx, snap.Backlog.Cursor); err != nil || len(b.Events) != 0 {
		t.Fatalf("backlog handoff = %+v, %v", b, err)
	}
	if err := log.Append(ctx, &eventlog.Event{Event: "Next"}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("observed next event")
	err = log.FollowCursor(ctx, snap.Backlog.Cursor, time.Millisecond, func(e eventlog.Event) error {
		if e.Event != "Next" {
			t.Fatalf("duplicated captured backlog: %+v", e)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("follow handoff = %v", err)
	}
}

func TestSnapshotOverlappingStateEventsConvergeWithExactSince(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	rec := session.Record{Agent: "claude", ID: "session", Main: "idle", Since: time.Unix(100, 11).UTC()}
	saveSnapshotRecord(t, ctx, store, log, rec)
	finalSince := time.Unix(500, 987654321).UTC()
	snap, err := takeSnapshot(ctx, log, func(ctx context.Context) (View, *attach.World, error) {
		rec.Main, rec.Since = "working", time.Unix(300, 22).UTC()
		saveSnapshotRecord(t, ctx, store, log, rec)
		rec.Main, rec.Since = "idle", finalSince
		saveSnapshotRecord(t, ctx, store, log, rec)
		// This image already contains both changes, while replay will first
		// move backward to working and then restore the later idle state.
		return snapshotView(t, store)
	})
	if err != nil || len(snap.Backlog.Events) != 2 {
		t.Fatalf("overlap capture = %+v, %v", snap, err)
	}
	state, since := snap.Sessions[0].State, snap.Sessions[0].Since
	for _, e := range snap.Backlog.Events {
		state, since = e.State, e.Since
	}
	if state != "idle" || since == nil || !since.Equal(finalSince) {
		t.Fatalf("overlap diverged: %s since %v", state, since)
	}
}

func TestSnapshotRetriesLostOverlapAndBoundsContinuousChurn(t *testing.T) {
	for _, churn := range []bool{false, true} {
		t.Run(map[bool]string{false: "one rotation race", true: "continuous churn"}[churn], func(t *testing.T) {
			log := eventlog.Open(t.TempDir())
			log.MaxBytes = 1
			snapshotPulse(t, context.Background(), log, 1)
			loads := 0
			snap, err := takeSnapshot(context.Background(), log, func(ctx context.Context) (View, *attach.World, error) {
				loads++
				if churn || loads == 1 {
					snapshotPulse(t, ctx, log, 3)
				}
				return Build(snapshotWorld(), nil), snapshotWorld(), nil
			})
			if churn {
				if !errors.Is(err, eventlog.ErrHistoryUnavailable) || loads != snapshotAttempts {
					t.Fatalf("unbounded churn: %d loads, %v", loads, err)
				}
			} else if err != nil || loads != 2 || !snap.Backlog.Cursor.IsSet() {
				t.Fatalf("retry capture = %+v, %d loads, %v", snap, loads, err)
			}
		})
	}
}

func TestCopiedSnapshotBacklogSurvivesRotationButFollowRefusesGap(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 1
	snapshotPulse(t, context.Background(), log, 1)
	snap, err := takeSnapshot(context.Background(), log, func(ctx context.Context) (View, *attach.World, error) {
		snapshotPulse(t, ctx, log, 1)
		return Build(snapshotWorld(), nil), snapshotWorld(), nil
	})
	if err != nil || len(snap.Backlog.Events) != 1 {
		t.Fatalf("capture = %+v, %v", snap, err)
	}
	copied := snap.Backlog.Events[0].Seq
	snapshotPulse(t, context.Background(), log, 4)
	if snap.Backlog.Events[0].Seq != copied {
		t.Fatal("rotation altered copied backlog")
	}
	err = log.FollowCursor(context.Background(), snap.Backlog.Cursor, time.Millisecond, func(eventlog.Event) error {
		t.Fatal("follow crossed a gap after snapshot publication")
		return nil
	})
	if !errors.Is(err, eventlog.ErrHistoryUnavailable) {
		t.Fatalf("publication handoff = %v", err)
	}
}

func TestSnapshotIncludesPendingStateAndLaterRepairPreservesSince(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	since := time.Unix(5, 123456789).UTC()
	rec := session.Record{Agent: "claude", ID: "pending", Main: "idle", Since: since, Change: "Stop"}
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.Put(rec) }); err != nil {
		t.Fatal(err)
	}
	snap, err := takeSnapshot(ctx, log, func(context.Context) (View, *attach.World, error) {
		return snapshotView(t, store)
	})
	if err != nil || len(snap.Sessions) != 1 || len(snap.Backlog.Events) != 0 {
		t.Fatalf("pending snapshot = %+v, %v", snap, err)
	}
	err = store.Lock(ctx, rec.Agent, rec.ID, func() error {
		_, _, err := store.Current(ctx, log, rec.Agent, rec.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	backlog, err := log.Replay(ctx, snap.Backlog.Cursor)
	if err != nil || len(backlog.Events) != 1 {
		t.Fatalf("pending repair = %+v, %v", backlog, err)
	}
	e := backlog.Events[0]
	if e.Since == nil || !e.Since.Equal(since) || e.Time.Equal(since) || !snap.Sessions[0].Since.Equal(*e.Since) {
		t.Fatalf("repair changed state since: snapshot=%v, event=%+v", snap.Sessions[0].Since, e)
	}
}

func TestSnapshotHeadHonorsParentDeadlineWhileLogLocked(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	held, release := make(chan struct{}), make(chan struct{})
	go func() { _ = log.WithLock(context.Background(), func() error { close(held); <-release; return nil }) }()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := takeSnapshot(ctx, log, func(context.Context) (View, *attach.World, error) {
		t.Fatal("loaded state after its cursor acquisition expired")
		return View{}, nil, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("snapshot ignored deadline: %v", err)
	}
}

func TestSnapshotPendingEndCannotResurrectAfterRepair(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	rec := session.Record{Agent: "claude", ID: "ending", Main: "idle", Since: time.Unix(50, 0).UTC()}
	saveSnapshotRecord(t, ctx, store, log, rec)
	rec.Ended = eventlog.End
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.Put(rec) }); err != nil {
		t.Fatal(err)
	}
	snap, err := takeSnapshot(ctx, log, func(context.Context) (View, *attach.World, error) { return snapshotView(t, store) })
	if err != nil || len(snap.Sessions) != 0 {
		t.Fatalf("pending end snapshot = %+v, %v", snap, err)
	}
	err = store.Lock(ctx, rec.Agent, rec.ID, func() error {
		_, _, err := store.Current(ctx, log, rec.Agent, rec.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := log.Replay(ctx, snap.Backlog.Cursor)
	if err != nil || len(batch.Events) != 1 || !batch.Events[0].Ended() {
		t.Fatalf("pending end repair = %+v, %v", batch, err)
	}
	if _, exists, err := store.Get(rec.Agent, rec.ID); err != nil || exists {
		t.Fatalf("ended session survived repair: %v, %v", exists, err)
	}
}
