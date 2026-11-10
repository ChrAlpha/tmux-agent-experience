package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	elsewhere := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(elsewhere, []byte(`{"agent":"claude","id":"link"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	recs, faults, err := s.All(ctx)
	if err != nil || len(recs) != 1 || recs[0].ID != good.ID || len(faults) != 2 {
		t.Fatalf("All = %+v, %+v, %v", recs, faults, err)
	}
	var fault *RecordError
	if _, ok, err := s.Get(ctx, proc.Claude, "bad"); ok || !errors.As(err, &fault) || fault.Category != "decode" {
		t.Fatalf("Get of a damaged record: %v, %v", ok, err)
	}
	if _, ok, err := s.Get(ctx, proc.Claude, "link"); ok || !errors.As(err, &fault) {
		t.Fatalf("Get through a symlink: %v, %v", ok, err)
	}
	if err := s.Put(ctx, Record{Agent: proc.Claude, ID: "bad", Main: "idle"}); err != nil {
		t.Fatal(err)
	}
	if rec, ok, err := s.Get(ctx, proc.Claude, "bad"); err != nil || !ok || rec.Main != "idle" {
		t.Fatalf("rewritten record: %+v, %v, %v", rec, ok, err)
	}
	if b, _ := os.ReadFile(elsewhere); string(b) != `{"agent":"claude","id":"link"}` {
		t.Fatalf("a symlink's target changed: %s", b)
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
