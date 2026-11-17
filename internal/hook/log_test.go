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
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
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
		err := Run(ctx, Input{Agent: "claude", PID: 0, Log: log, Now: func() time.Time { return at },
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

// A hook whose process table cannot be read still records the state, and
// leaves the host and inbox it had recorded alone.
func TestHookRecordsStateWithoutAProcessTable(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	host := session.Host{PID: 123, Start: "s123", Role: proc.Service}
	inbox := &session.Inbox{Path: "/tmp/inbox.sock", PID: 123, Start: "s123"}
	rec := session.Record{Agent: proc.Claude, ID: "state-first", Main: adapter.Idle, Host: host, Inbox: inbox}
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	previous := loadLineage
	loadLineage = func(context.Context, int) (*proc.Table, error) { return nil, errors.New("process table unreadable") }
	t.Cleanup(func() { loadLineage = previous })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload := `{"session_id":"state-first","hook_event_name":"UserPromptSubmit","prompt_id":"turn"}`
	if err := Run(ctx, Input{Agent: proc.Claude, Payload: strings.NewReader(payload), Log: log, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), proc.Claude, "state-first")
	if err != nil || !ok {
		t.Fatalf("record: %+v %v %v", got, ok, err)
	}
	if got.State() != adapter.Working || got.Host != host || !session.SameInbox(got.Inbox, inbox) {
		t.Fatalf("state=%s host=%+v inbox=%+v, want working, %+v, %+v", got.State(), got.Host, got.Inbox, host, inbox)
	}
	events, _, err := log.Since(context.Background(), 0)
	if err != nil || len(events) != 1 || events[0].Session != "state-first" || events[0].State != adapter.Working {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

func TestHookFailureLogIsBoundedMetadataAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hook.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxErrLog), 0o600); err != nil {
		t.Fatal(err)
	}
	RecordError(context.Background(), root, "notify", errors.New("private command output/payload sentinel"))
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxErrLog || !bytes.Contains(data, []byte("notify failed")) || bytes.Contains(data, []byte("sentinel")) {
		t.Fatalf("not bounded metadata: bytes=%d err=%v", len(data), err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	RecordError(canceled, root, "claude", errors.New("ignored"))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("canceled logger wrote after its budget", err)
	}
}
