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
