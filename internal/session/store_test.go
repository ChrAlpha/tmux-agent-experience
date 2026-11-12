package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// A record replaced while it is read is read whole, old or new: views and
// lookups taken during writes see every session.
func TestReadsSurviveConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	store := Store{Dir: t.TempDir()}
	const records = 8
	put := func(i, n int) error {
		rec := Record{Agent: proc.Codex, ID: fmt.Sprintf("thread-%d", i), Main: "idle", LastHookAt: time.Unix(int64(n), 0).UTC()}
		return store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.Put(ctx, rec) })
	}
	for i := range records {
		if err := put(i, 0); err != nil {
			t.Fatal(err)
		}
	}
	stop, published := make(chan struct{}), make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				published <- n
				return
			default:
			}
			if err := put(n%records, n+1); err != nil {
				t.Error(err)
				published <- n
				return
			}
			n++
		}
	}()
	defer func() {
		close(stop)
		if n := <-published; n == 0 {
			t.Error("no replacement overlapped the reads")
		}
	}()
	for range 300 {
		if all, bad, err := store.All(ctx); err != nil || len(bad) != 0 || len(all) != records {
			t.Fatalf("view during replacement: %d records, %d unreadable, %v", len(all), len(bad), err)
		}
		if _, ok, err := store.Get(ctx, proc.Codex, "thread-3"); err != nil || !ok {
			t.Fatalf("record during replacement: %v %v", ok, err)
		}
	}
}

// A record that cannot be read is left out of views, as though the session
// were gone, and reported; the session's next write replaces it.
func TestUnreadableRecordIsLeftOutAndReported(t *testing.T) {
	ctx := context.Background()
	s := Store{Dir: t.TempDir()}
	good := play(t)
	if err := s.Put(ctx, good); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Dir, "sessions", proc.Claude)
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, faults, err := s.All(ctx)
	if err != nil || len(recs) != 1 || recs[0].ID != good.ID || len(faults) != 1 {
		t.Fatalf("All = %+v, %+v, %v", recs, faults, err)
	}
	var fault *RecordError
	if _, ok, err := s.Get(ctx, proc.Claude, "bad"); ok || !errors.As(err, &fault) || fault.Category != "decode" {
		t.Fatalf("Get of a damaged record: %v, %v", ok, err)
	}
	if err := s.Put(ctx, Record{Agent: proc.Claude, ID: "bad", Main: "idle"}); err != nil {
		t.Fatal(err)
	}
	if rec, ok, err := s.Get(ctx, proc.Claude, "bad"); err != nil || !ok || rec.Main != "idle" {
		t.Fatalf("rewritten record: %+v, %v, %v", rec, ok, err)
	}
}

// Writes go through one temporary per stripe of the session lock pool: a
// temporary a writer that died left behind is reused, and however many
// sessions come and go, no more temporaries than stripes remain.
func TestTemporariesAreBoundedAndReused(t *testing.T) {
	ctx := context.Background()
	s := Store{Dir: t.TempDir()}
	rec := Record{Agent: proc.Claude, ID: "s1", Main: "idle"}
	dir := filepath.Join(s.Dir, "sessions", proc.Claude)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(dir, ".tmp-"+hex2(sessionKey(rec.Agent, rec.ID)[0]))
	if err := os.WriteFile(left, []byte("a writer died here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover temporary was not reused: %v", err)
	}
	if got, ok, err := s.Get(ctx, rec.Agent, rec.ID); err != nil || !ok || got.Main != "idle" {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if temps, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(temps) != 0 {
		t.Fatalf("temporaries left after a write: %v", temps)
	}
}

func hex2(b byte) string { return string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15]) }

// An end deletes the record before logging it.
func TestEndDeletesThenLogs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	rec := play(t)
	rec.Cwd = "/w"
	if err := s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.End(ctx, log, rec, eventlog.Event{Event: eventlog.Exit}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, rec.Agent, rec.ID); ok {
		t.Fatal("an ended session's record remains")
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil || len(events) != 1 || events[0].Event != eventlog.Exit || events[0].Session != rec.ID || events[0].Cwd != "/w" {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

func TestCanceledReads(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Get(ctx, proc.Claude, "s1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get = %v", err)
	}
	if _, _, err := s.All(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("All = %v", err)
	}
}

// Sessions that share a lock stripe share its temporary too; their writes
// take turns, and every record stays whole.
func TestSessionsOfOneStripeWriteInTurn(t *testing.T) {
	ctx := context.Background()
	s := Store{Dir: t.TempDir()}
	ids := []string{"a0"}
	for i := 1; len(ids) < 2; i++ {
		id := "a" + strconv.Itoa(i)
		if sessionKey(proc.Claude, id)[0] == sessionKey(proc.Claude, ids[0])[0] {
			ids = append(ids, id)
		}
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			for i := range 50 {
				rec := Record{Agent: proc.Claude, ID: id, Main: "idle", Turn: strconv.Itoa(i)}
				if err := s.Lock(ctx, rec.Agent, rec.ID, func() error { return s.Put(ctx, rec) }); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		if rec, ok, err := s.Get(ctx, proc.Claude, id); err != nil || !ok || rec.ID != id || rec.Turn != "49" {
			t.Fatalf("%s: %+v %v %v", id, rec, ok, err)
		}
	}
}

// A session keeps a bounded number of prompts; the oldest go first, and
// their requests with them, which tells the answer hooks waiting on them to
// leave the decision to the agent's dialog.
func TestPromptsBeyondTheBoundCloseTheirRequests(t *testing.T) {
	rec := play(t)
	for i := range maxPending + 1 {
		tr := askFor("", "tool"+strconv.Itoa(i))
		tr.Request, tr.Waits = "r"+strconv.Itoa(i), true
		rec, _ = Apply(rec, tr, t0)
	}
	if len(rec.Pending) != maxPending || rec.Pending[0].Tool != "tool1" {
		t.Fatalf("%d prompts kept, the oldest %+v", len(rec.Pending), rec.Pending[0])
	}
	if _, open := rec.Request("r0"); open || len(rec.Requests) != maxPending {
		t.Fatalf("requests %d, the dropped prompt's still open: %v", len(rec.Requests), open)
	}
	if rec.State() != "blocked" {
		t.Fatalf("state %s", rec.State())
	}
}
