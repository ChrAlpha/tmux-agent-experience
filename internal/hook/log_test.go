package hook

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// A hook that changes nothing but contact records it at most every
// ContactInterval, without changing the state's age or logging an event.
func TestHookContactIsRecordedWithoutChangingStateAge(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	since := time.Unix(10, 123).UTC()
	first := time.Unix(20, 456).UTC()
	rec := session.Record{Agent: "claude", ID: "thread", Main: "working", Turn: "turn", Since: since, LastHookAt: first,
		Calls: []adapter.Call{{ID: "call", Tool: "Bash"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	hook := func(at time.Time) {
		t.Helper()
		err := RunContext(ctx, Input{Agent: "claude", PID: 0, Log: log, Now: func() time.Time { return at },
			Payload: strings.NewReader(`{"session_id":"thread","hook_event_name":"PreToolUse","prompt_id":"turn","tool_name":"Bash","tool_use_id":"call"}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	hook(first.Add(ContactInterval / 2))
	if got, _, _ := store.Get(ctx, "claude", "thread"); !got.LastHookAt.Equal(first) {
		t.Fatalf("contact within the interval was written: %v", got.LastHookAt)
	}
	later := first.Add(2 * ContactInterval)
	hook(later)
	got, ok, err := store.Get(ctx, "claude", "thread")
	if err != nil || !ok || !got.LastHookAt.Equal(later) || !got.Since.Equal(since) || got.State() != "working" {
		t.Fatalf("contact changed state/age or was not saved: %+v %v", got, err)
	}
	if events, _, err := log.Since(ctx, 0); err != nil || len(events) != 0 {
		t.Fatalf("contact alone was logged: %+v %v", events, err)
	}
}
func TestHookFailureLogIsBoundedMetadataAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hook.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxErrLog), 0o600); err != nil {
		t.Fatal(err)
	}
	RecordErrorContext(context.Background(), root, "notify", errors.New("private command output/payload sentinel"))
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxErrLog || !bytes.Contains(data, []byte("notify failed")) || bytes.Contains(data, []byte("sentinel")) {
		t.Fatalf("not bounded metadata: bytes=%d err=%v", len(data), err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	RecordErrorContext(canceled, root, "claude", errors.New("ignored"))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("canceled logger wrote after its budget", err)
	}
}
