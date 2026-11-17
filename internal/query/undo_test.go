package query

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/sockettest"
)

// An inbox that answers that it took nothing leaves no mark behind: the
// retry its next: line advises is delivered rather than refused busy.
func TestNotTakenDeliveryLeavesNoMark(t *testing.T) {
	sock := filepath.Join(sockettest.Dir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte("{\"ok\":false}\n"))
			_ = c.Close()
		}
	}()
	tb, err := proc.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	self, _ := tb.Get(os.Getpid())
	state := t.TempDir()
	log := eventlog.Open(state)
	store := session.Store{Dir: state}
	rec := session.Record{Agent: proc.Pi, ID: "s1", Main: adapter.Working,
		Host:  session.Host{PID: os.Getpid(), Start: self.Start, Role: proc.Terminal},
		Inbox: &session.Inbox{Path: sock, PID: os.Getpid(), Start: self.Start}}
	ctx := t.Context()
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		v, _, err := Load(ctx, store)
		if err != nil {
			t.Fatal(err)
		}
		s, ok := v.Session(rec.Agent, rec.ID)
		if !ok {
			t.Fatal("no session")
		}
		_, err = throughInbox(ctx, log, v, s, "hello", Caller{}, liveGate, false)
		var notTaken *NotTakenError
		if !errors.As(err, &notTaken) {
			t.Fatalf("attempt %d: %v, want the inbox's refusal to take it", attempt, err)
		}
		got, _, err := store.Get(ctx, rec.Agent, rec.ID)
		if err != nil || !got.Queued.IsZero() {
			t.Fatalf("attempt %d left queued %v: %v", attempt, got.Queued, err)
		}
	}
}

// Taking a mark back leaves one a later delivery set.
func TestUnmarkKeepsALaterMark(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	s := Session{Agent: proc.Pi, ID: "s1"}
	t1, t2 := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
	prior := session.Record{Agent: s.Agent, ID: s.ID}
	marked := prior
	marked.Sent, marked.Halted = t1, t1
	later := marked
	later.Sent = t2
	if err := store.Put(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if err := unmark(store, s, prior, marked)(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Get(t.Context(), s.Agent, s.ID)
	if err != nil || !got.Sent.Equal(t2) || !got.Halted.IsZero() {
		t.Fatalf("after unmark: sent %v halted %v, %v; want the later sent kept and this halt taken back", got.Sent, got.Halted, err)
	}
}

// A delivery that proved it took nothing takes back only its own mark: a
// later delivery's mark, set in the same second after a hook cleared the
// first, stays, so a third delivery is still held off by it.
func TestUndoKeepsALaterMarkOfTheSameSecond(t *testing.T) {
	sock := filepath.Join(sockettest.Dir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	release := make(chan struct{})
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(first bool) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				if first {
					<-release
					_, _ = c.Write([]byte("{\"ok\":false}\n"))
					return
				}
				_, _ = c.Write([]byte("{\"ok\":true}\n"))
			}(n == 0)
		}
	}()
	tb, err := proc.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	self, _ := tb.Get(os.Getpid())
	state := t.TempDir()
	log := eventlog.Open(state)
	store := session.Store{Dir: state}
	rec := session.Record{Agent: proc.Pi, ID: "s1", Main: adapter.Working,
		Host:  session.Host{PID: os.Getpid(), Start: self.Start, Role: proc.Terminal},
		Inbox: &session.Inbox{Path: sock, PID: os.Getpid(), Start: self.Start}}
	ctx := t.Context()
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	send := func() error {
		v, _, err := Load(ctx, store)
		if err != nil {
			return err
		}
		s, ok := v.Session(rec.Agent, rec.ID)
		if !ok {
			return errors.New("no session")
		}
		_, err = throughInbox(ctx, log, v, s, "hello", Caller{}, liveGate, false)
		return err
	}
	queued := func() time.Time {
		t.Helper()
		got, _, err := store.Get(ctx, rec.Agent, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Queued
	}
	first := make(chan error, 1)
	go func() { first <- send() }()
	for deadline := time.Now().Add(5 * time.Second); queued().IsZero(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first delivery set no mark")
		}
	}
	// The main agent reports, which confirms the first prompt.
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error {
		cur, _, err := store.Get(ctx, rec.Agent, rec.ID)
		if err != nil {
			return err
		}
		cur.Queued = time.Time{}
		return store.Put(ctx, cur)
	}); err != nil {
		t.Fatal(err)
	}
	if err := send(); err != nil {
		t.Fatalf("the second delivery: %v", err)
	}
	second := queued()
	close(release)
	var notTaken *NotTakenError
	if err := <-first; !errors.As(err, &notTaken) {
		t.Fatalf("the first delivery: %v, want the inbox's refusal to take it", err)
	}
	if got := queued(); got.IsZero() || !got.Equal(second) {
		t.Fatalf("queued %v after the first delivery's undo, want the second's %v", got, second)
	}
}
