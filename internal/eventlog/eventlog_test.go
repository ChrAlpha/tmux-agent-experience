package eventlog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
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

func head(t *testing.T, l *Log) int64 {
	t.Helper()
	seq, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestAppendAssignsSequence(t *testing.T) {
	l := Open(t.TempDir())
	if seq := head(t, l); seq != 0 {
		t.Fatalf("empty log head = %d", seq)
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
	if got := head(t, l); got != base+3 {
		t.Fatalf("head = %d, want %d", got, base+3)
	}
	if evs, complete, _ := l.Since(bg, base); len(evs) != 3 || !complete {
		t.Fatalf("Since(base) = %d events, complete=%v", len(evs), complete)
	}
	if evs, complete, _ := l.Since(bg, base+3); len(evs) != 0 || !complete {
		t.Fatalf("Since(last) = %v, %v", evs, complete)
	}
	if _, complete, _ := l.Since(bg, base+4); complete {
		t.Fatal("a cursor ahead of the log is complete")
	}
}

func TestConcurrentAppendsAreContiguous(t *testing.T) {
	l := Open(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); appendN(t, l, 25) }()
	}
	wg.Wait()
	evs, _, err := l.Since(bg, 0)
	if err != nil || len(evs) != 200 {
		t.Fatalf("Since(0) = %d events, err=%v", len(evs), err)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			t.Fatalf("seq %d follows %d", evs[i].Seq, evs[i-1].Seq)
		}
	}
}

// Rotation keeps numbering; a cursor older than the retained history is
// reported incomplete rather than answered with a suffix.
func TestRotationKeepsSequenceAndDetectsLoss(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 600
	appendN(t, l, 40)
	evs, _, err := l.Since(bg, 0)
	if err != nil || len(evs) == 0 || len(evs) >= 40 {
		t.Fatalf("retained %d of 40 events, err=%v", len(evs), err)
	}
	first, last := evs[0].Seq, evs[len(evs)-1].Seq
	if last != head(t, l) {
		t.Fatalf("head %d, last retained %d", head(t, l), last)
	}
	if _, complete, _ := l.Since(bg, first-1); !complete {
		t.Fatal("a cursor just before the retained history is incomplete")
	}
	if got, complete, _ := l.Since(bg, first-2); complete || len(got) != len(evs) {
		t.Fatalf("a cursor older than the retained history: %d events, complete=%v", len(got), complete)
	}
}

// A line a writer that died left unfinished is skipped, and its number is
// used by the next writer: readers see numbers without gaps.
func TestUnfinishedLineIsEndedAndSkipped(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 2)
	before := head(t, l)
	f, err := os.OpenFile(filepath.Join(l.Dir, currentName), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := head(t, l); got != before {
		t.Fatalf("head with an unfinished line = %d, want %d", got, before)
	}
	appendN(t, l, 1)
	evs, complete, err := l.Since(bg, before-2)
	if err != nil || !complete || len(evs) != 3 || evs[2].Seq != before+1 {
		t.Fatalf("after an unfinished line: %v complete=%v err=%v", evs, complete, err)
	}
}

// A writer that died after its event's last byte but before its newline
// logged the event: the next one starts a line of its own after it.
func TestEventWithoutNewlineCounts(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	seq := head(t, l)
	f, err := os.OpenFile(filepath.Join(l.Dir, currentName), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":` + itoa(seq+1) + `,"agent":"claude","session":"s","event":"Stop"}`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	appendN(t, l, 1)
	evs, complete, err := l.Since(bg, seq)
	if err != nil || !complete || len(evs) != 2 || evs[0].Seq != seq+1 || evs[1].Seq != seq+2 {
		t.Fatalf("after an event without newline: %v complete=%v err=%v", evs, complete, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A log recreated since a cursor is never a continuation of it, even when
// its numbers run on.
func TestRecreatedLogIsIncomplete(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 3)
	seq := head(t, l)
	for _, name := range []string{currentName, rotatedName} {
		_ = os.Remove(filepath.Join(l.Dir, name))
	}
	if _, complete, _ := l.Since(bg, seq); complete {
		t.Fatal("an emptied log continues a cursor into it")
	}
	time.Sleep(time.Millisecond)
	appendN(t, l, 1)
	if _, complete, _ := l.Since(bg, seq); complete {
		t.Fatal("a recreated log continues a cursor into the old one")
	}
}

// A tail reads only what was appended, follows a rotation without a loss,
// and reports one it could not follow.
func TestTail(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 2000
	appendN(t, l, 2)
	tail := l.Tail(head(t, l))
	defer tail.Close()
	if evs, complete, err := tail.Next(bg); err != nil || !complete || len(evs) != 0 {
		t.Fatalf("an idle tail: %v %v %v", evs, complete, err)
	}
	got := 0
	for range 30 {
		appendN(t, l, 1) // rotates every few events
		evs, complete, err := tail.Next(bg)
		if err != nil || !complete || len(evs) != 1 {
			t.Fatalf("after %d events: %v %v %v", got, evs, complete, err)
		}
		got++
	}
	if tail.Seq() != head(t, l) {
		t.Fatalf("tail at %d, log at %d", tail.Seq(), head(t, l))
	}
	// The file a tail holds survives its rotation away, so two rotations
	// between looks lose nothing; a file rotated away unread does.
	appendN(t, l, 30)
	if evs, complete, err := tail.Next(bg); err != nil || !complete || len(evs) != 30 {
		t.Fatalf("a tail past two rotations: %d events, complete=%v err=%v", len(evs), complete, err)
	}
	appendN(t, l, 100)
	if _, complete, err := tail.Next(bg); err != nil || complete {
		t.Fatalf("a tail past many rotations: complete=%v err=%v", complete, err)
	}
	tail.Close()
	fresh := l.Tail(head(t, l))
	defer fresh.Close()
	appendN(t, l, 1)
	if evs, complete, err := fresh.Next(bg); err != nil || !complete || len(evs) != 1 {
		t.Fatalf("a new tail: %v %v %v", evs, complete, err)
	}
}

func TestLockHonoursDeadline(t *testing.T) {
	l := Open(t.TempDir())
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = l.WithLock(bg, func() error { close(held); <-release; return nil })
	}()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(bg, 30*time.Millisecond)
	defer cancel()
	if err := l.Append(ctx, &Event{Event: "Stop"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("append under a held lock: %v", err)
	}
}

// A log file replaced by a FIFO fails instead of blocking a hook.
func TestSpecialFileDoesNotBlock(t *testing.T) {
	l := Open(t.TempDir())
	if err := syscall.Mkfifo(filepath.Join(l.Dir, currentName), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	if err := l.Append(ctx, &Event{Event: "Stop"}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("append to a FIFO: %v", err)
	}
	if _, _, err := l.Since(ctx, 0); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read of a FIFO: %v", err)
	}
}

// A tail that follows from the end of a log vouches for what comes next, the
// first events of a fresh log too, across rotations it sees, and for nothing
// once files it never read rotated away.
func TestFollowFromTheEnd(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 500
	tail, err := l.Follow(bg)
	if err != nil || tail.Seq() != 0 {
		t.Fatalf("Follow of an empty log: %v %v", tail, err)
	}
	defer tail.Close()
	if evs, complete, err := tail.Next(bg); err != nil || !complete || len(evs) != 0 {
		t.Fatalf("an empty log, still empty: %v %v %v", evs, complete, err)
	}
	appendN(t, l, 3)
	if evs, complete, err := tail.Next(bg); err != nil || !complete || len(evs) != 3 {
		t.Fatalf("the first events of a fresh log: %d %v %v", len(evs), complete, err)
	}
	for range 3 {
		appendN(t, l, 3) // a rotation between looks
		if evs, complete, err := tail.Next(bg); err != nil || !complete || len(evs) != 3 {
			t.Fatalf("across a rotation: %d %v %v", len(evs), complete, err)
		}
	}
	appendN(t, l, 30) // several rotations: files the tail never read are gone
	if _, complete, err := tail.Next(bg); err != nil || complete {
		t.Fatalf("past several rotations: complete=%v err=%v", complete, err)
	}

	fresh := Open(t.TempDir())
	fresh.MaxBytes = 500
	early, err := fresh.Follow(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	appendN(t, fresh, 30)
	if _, complete, err := early.Next(bg); err != nil || complete {
		t.Fatalf("a fresh log rotated away before the first look: complete=%v err=%v", complete, err)
	}

	mid, err := l.Follow(bg)
	if err != nil || mid.Seq() != head(t, l) {
		t.Fatalf("Follow mid-log at %d, head %d: %v", mid.Seq(), head(t, l), err)
	}
	defer mid.Close()
	appendN(t, l, 1)
	if evs, complete, err := mid.Next(bg); err != nil || !complete || len(evs) != 1 || evs[0].Seq != mid.Seq() {
		t.Fatalf("after Follow mid-log: %v %v %v", evs, complete, err)
	}
}

// A file emptied under a tail, which keeps its inode, costs a snapshot
// rather than events.
func TestTailNoticesAFileItMistookForItsOwn(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 3)
	tail, err := l.Follow(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer tail.Close()
	// Keep the inode, lose the history: the file is emptied and refilled with
	// fewer bytes than the tail had read.
	path := filepath.Join(l.Dir, currentName)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)
	if _, complete, err := tail.Next(bg); err != nil || complete {
		t.Fatalf("a refilled file passed for the tail's own: complete=%v err=%v", complete, err)
	}
}
