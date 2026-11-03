package eventlog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Append(bg, &Event{Agent: "claude", Session: "s", Event: "Stop", State: "idle"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppendAssignsSequence(t *testing.T) {
	l := Open(t.TempDir())
	if seq, _ := l.Seq(); seq != 0 {
		t.Fatalf("empty log seq = %d", seq)
	}
	if evs, complete, _ := l.Since(bg, 0); len(evs) != 0 || !complete {
		t.Fatalf("empty log Since(0) = %v, %v", evs, complete)
	}
	appendN(t, l, 3)
	evs, _, err := l.Since(bg, 0)
	if err != nil || len(evs) != 3 {
		t.Fatalf("Since(0) = %d events, err=%v", len(evs), err)
	}
	base := evs[0].Seq - 1
	if base < time.Now().Add(-time.Hour).UnixMicro() {
		t.Fatalf("log is not numbered from the current time: first seq %d", evs[0].Seq)
	}
	for i, e := range evs {
		if e.Seq != base+int64(i+1) || e.Time.IsZero() {
			t.Fatalf("event %d: seq %d time %v", i, e.Seq, e.Time)
		}
	}
	if evs, complete, _ := l.Since(bg, base); len(evs) != 3 || !complete {
		t.Fatalf("Since(base) = %d events, complete=%v", len(evs), complete)
	}
	if evs, complete, _ := l.Since(bg, base+3); len(evs) != 0 || !complete {
		t.Fatalf("Since(last) = %v, %v", evs, complete)
	}
}

func TestConcurrentAppendsAreUnique(t *testing.T) {
	l := Open(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); appendN(t, l, 25) }()
	}
	wg.Wait()
	evs, _, _ := l.Since(bg, 0)
	if len(evs) != 200 {
		t.Fatalf("got %d events", len(evs))
	}
	for i, e := range evs {
		if e.Seq != evs[0].Seq+int64(i) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

func TestRotationKeepsSequenceAndDetectsLoss(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 600
	appendN(t, l, 30)
	seq, _ := l.Seq()
	evs, complete, _ := l.Since(bg, seq-2)
	if !complete || len(evs) != 2 || evs[0].Seq != seq-1 {
		t.Fatalf("recent range: %d events, complete=%v", len(evs), complete)
	}
	if _, complete, _ := l.Since(bg, seq-29); complete {
		t.Fatal("claims completeness after rotation dropped events")
	}
}

func TestCursorFromAnotherLogIsIncomplete(t *testing.T) {
	old := Open(t.TempDir())
	appendN(t, old, 3)
	cursor, _ := old.Seq()
	time.Sleep(2 * time.Millisecond)
	recreated := Open(t.TempDir()) // e.g. after tmag uninstall --purge
	appendN(t, recreated, 5)
	if _, complete, _ := recreated.Since(bg, cursor); complete {
		t.Fatal("a cursor from a purged log was accepted")
	}
	if _, complete, _ := recreated.Since(bg, cursor+1<<40); complete {
		t.Fatal("a cursor beyond the log was accepted")
	}
}

func TestHoleIsTolerated(t *testing.T) {
	dir := t.TempDir()
	l := Open(dir)
	appendN(t, l, 2)
	seq, _ := l.Seq()
	// A writer died after committing seq+1 but before writing its line.
	if err := l.writeSeq(seq + 1); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)
	evs, complete, _ := l.Since(bg, seq-1)
	if !complete || len(evs) != 2 || evs[1].Seq != seq+2 {
		t.Fatalf("Since across a hole = %+v, complete=%v", evs, complete)
	}
}

func TestSeqSurvivesMissingSeqFile(t *testing.T) {
	dir := t.TempDir()
	l := Open(dir)
	appendN(t, l, 4)
	before, _ := l.Seq()
	os.Remove(filepath.Join(dir, seqName))
	appendN(t, l, 1)
	if seq, _ := l.Seq(); seq != before+1 {
		t.Fatalf("seq = %d, want %d", seq, before+1)
	}
}

func TestLockHonoursDeadline(t *testing.T) {
	l := Open(t.TempDir())
	held := make(chan struct{})
	release := make(chan struct{})
	go func() { _ = l.WithLock(bg, func() error { close(held); <-release; return nil }) }()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.Append(ctx, &Event{Session: "s"}); err == nil {
		t.Fatal("append succeeded while the lock was held")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s for a contended lock", waited)
	}
}

func TestFollowAcrossRotation(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 700
	appendN(t, l, 2)
	start, _ := l.Seq()

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	got := make(chan int64, 100)
	go func() {
		_ = l.Follow(ctx, start-1, 10*time.Millisecond, func(e Event) error {
			got <- e.Seq
			return nil
		})
	}()
	go func() {
		for i := 0; i < 20; i++ {
			appendN(t, l, 1)
			time.Sleep(6 * time.Millisecond)
		}
	}()
	for want := start; want <= start+20; want++ {
		select {
		case seq := <-got:
			if seq != want {
				t.Fatalf("got seq %d, want %d", seq, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
}
