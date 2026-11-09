package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestHookContactIsPublishedWithoutChangingStateAge(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	since := time.Unix(10, 123).UTC()
	contact := time.Unix(20, 456).UTC()
	rec := session.Record{Agent: "claude", ID: "thread", Main: "working", Turn: "turn", Since: since}
	rec.Logged = rec.Reports()
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunContext(ctx, Input{Agent: "claude", PID: 0, Log: log, Now: func() time.Time { return contact }, Payload: strings.NewReader(`{"session_id":"thread","hook_event_name":"PreToolUse","prompt_id":"turn","tool_name":"Bash","tool_use_id":"call"}`)})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get("claude", "thread")
	if err != nil || !ok || !got.LastHookAt.Equal(contact) || !got.Since.Equal(since) || got.State() != "working" {
		t.Fatalf("contact changed state/age or was not saved: %+v %v", got, err)
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil || len(events) != 1 || !events[0].LastHookAt.Equal(contact) || events[0].Since == nil || !events[0].Since.Equal(since) {
		t.Fatalf("contact event incomplete: %+v %v", events, err)
	}
}
func TestHookFailureLogIsBoundedMetadataAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	if _, err := coord.Current(context.Background(), root); err != nil {
		t.Fatal(err)
	}
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

// A change too large to record is refused whole: the session and the log
// stay as they were, and the failure is logged as its category alone.
func TestOversizeChangeIsRefusedAndLoggedAsItsCategory(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	rec := session.Record{Agent: "claude", ID: "thread", Main: "idle", Cwd: "/a", Since: time.Unix(10, 0).UTC()}
	rec.Logged = rec.Reports()
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "claude", "thread.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	head, err := log.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"session_id": "thread", "hook_event_name": "UserPromptSubmit", "prompt_id": "p1",
		"cwd": strings.Repeat("<", session.MaxRecordBytes/6+1)})
	if err != nil {
		t.Fatal(err)
	}
	err = RunContext(ctx, Input{Agent: "claude", PID: 0, Log: log, Payload: bytes.NewReader(payload)})
	if !errors.As(err, new(*session.LimitError)) {
		t.Fatalf("RunContext = %v", err)
	}
	after, _ := os.ReadFile(path)
	if now, err := log.Head(ctx); err != nil || now != head || !bytes.Equal(after, before) {
		t.Fatalf("a refused change was published: %v", err)
	}
	RecordErrorContext(ctx, root, "claude", err)
	data, err := os.ReadFile(filepath.Join(root, "hook.log"))
	if err != nil || !bytes.HasSuffix(data, []byte(" claude limit_exceeded\n")) || bytes.Contains(data, []byte("<")) {
		t.Fatalf("hook.log %q, %v", data, err)
	}
}
