package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

type cancelReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	return copy(p, `{"session_id":"deadline-test","hook_event_name":"SessionStart"}`), io.EOF
}

func TestCancellationDuringInputCannotMutate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := &cancelReader{cancel: cancel}
	dir := t.TempDir()
	err := RunContext(ctx, Input{Agent: proc.Claude, Payload: in, Log: eventlog.Open(dir), PID: os.Getpid()})
	if !errors.Is(err, context.Canceled) || in.reads != 1 {
		t.Fatalf("reads=%d err=%v", in.reads, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late session mutation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late log mutation: %v", err)
	}
}

// A malformed first hook must not turn a fresh deployment into legacy state.
func TestFirstDiagnosticAllowsNormalStateInitialization(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	RecordErrorContext(ctx, dir, "claude", errors.New("synthetic malformed hook"))
	if _, err := os.Stat(filepath.Join(dir, "hook.log")); err != nil {
		t.Fatal(err)
	}
	store := session.Store{Dir: dir}
	rec := session.Record{Agent: proc.Claude, ID: "healthy", Main: "idle"}
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.Put(ctx, rec) }); err != nil {
		t.Fatalf("healthy invocation after diagnostic: %v", err)
	}
	if records, _, err := store.All(ctx); err != nil || len(records) != 1 {
		t.Fatalf("normal read after diagnostic: %d %v", len(records), err)
	}
}
