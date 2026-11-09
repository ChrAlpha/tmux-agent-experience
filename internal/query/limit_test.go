package query

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// An answer whose record would exceed the record limit is refused before
// the waiting answer hook gets it: the agent's own dialog still decides.
func TestAnswerHookRefusesBeforeDeciding(t *testing.T) {
	dir, err := os.MkdirTemp("", "tmag-q-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ctx := context.Background()
	log, store := eventlog.Open(dir), session.Store{Dir: dir}
	id := session.NewRequestID()
	ln, err := answer.ListenContext(ctx, dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	waiting, stop := context.WithCancel(ctx)
	decided, done := make(chan []byte, 1), make(chan struct{})
	go func() {
		defer close(done)
		answer.Wait(waiting, ln, answer.Asked{Tool: "Bash"}, func(out []byte) error { decided <- out; return nil })
	}()
	defer func() { stop(); <-done }()

	// Blocked on the request, the session's record is exactly the limit.
	// Answered, the request goes, and the mark keeps the answer's event:
	// larger by far than the request.
	rec := session.Record{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Turn: "p1", Since: time.Unix(100, 0).UTC(),
		Pending: []adapter.Prompt{{}}, Requests: []session.Request{{ID: id, Hook: true}}}
	rec.Logged = rec.Reports()
	path := filepath.Join(dir, "sessions", proc.Claude, "s1.json")
	rec.Done = []string{"x"}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.Done = []string{strings.Repeat("x", 1+session.MaxRecordBytes-int(st.Size()))}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil || len(before) != session.MaxRecordBytes {
		t.Fatalf("record is %d bytes: %v", len(before), err)
	}
	head, err := log.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = answerHook(canceled, log, Session{Agent: proc.Claude, ID: "s1"}, id, answer.Decision{Behavior: answer.Allow}, Caller{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled answerHook = %v", err)
	}
	_, err = answerHook(ctx, log, Session{Agent: proc.Claude, ID: "s1"}, id, answer.Decision{Behavior: answer.Allow}, Caller{})
	if !errors.As(err, new(*session.LimitError)) {
		t.Fatalf("answerHook = %v", err)
	}
	select {
	case out := <-decided:
		t.Fatalf("the hook got a decision: %s", out)
	case <-time.After(300 * time.Millisecond):
	}
	if answer.Probe(ctx, dir, id) != answer.Alive {
		t.Fatal("the answer hook stopped waiting")
	}
	after, _ := os.ReadFile(path)
	if now, err := log.Head(ctx); err != nil || now != head || string(after) != string(before) {
		t.Fatalf("a refused answer was published: %v", err)
	}
}
