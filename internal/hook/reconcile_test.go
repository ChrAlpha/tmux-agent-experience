package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// The pane a session B attached to between the hook's process snapshot and
// its pane listing stays attached: reconciliation reads the process table
// again after the panes, so the fresher attachment has its client.
func TestReconciliationReadsProcessesAfterPanes(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := session.Record{Agent: proc.Claude, ID: "b-session", Main: adapter.Idle,
		Host: session.Host{PID: 200, Start: "s-b"}}
	if err := store.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	panes := []tmux.Pane{{Server: "r1", ID: "%1", PID: 100, Command: "claude", Options: map[string]string{
		"agent": "claude", "session": "b-session", "pid": "200", "start": "s-b",
		"evidence": "process", "state": "idle",
	}}}
	previousProcesses, previousPanes := loadProcesses, loadPanes
	t.Cleanup(func() { loadProcesses, loadPanes = previousProcesses, previousPanes })
	calls := 0
	loadProcesses = func(_ context.Context, w *attach.World) error {
		calls++
		ps := []proc.Process{{PID: 100, PPID: 1, Comm: "sh", Args: "sh"}}
		if calls > 1 {
			ps = append(ps, proc.Process{PID: 200, PPID: 100, Comm: "claude", Args: "claude", Start: "s-b"})
		}
		w.Proc = proc.NewTable(ps)
		return nil
	}
	loadPanes = func(_ context.Context, w *attach.World) error {
		if calls != 1 {
			t.Fatalf("panes listed after the hook refreshed the process table")
		}
		w.Panes = panes
		return nil
	}

	payload := `{"session_id":"a-session","hook_event_name":"UserPromptSubmit","prompt_id":"turn"}`
	if err := RunContext(ctx, Input{Agent: proc.Claude, Payload: strings.NewReader(payload), Log: log, PID: 0}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("process table read %d times, want one before and one after the panes", calls)
	}
	if _, ok, err := store.Get(ctx, proc.Claude, "b-session"); !ok || err != nil {
		t.Fatalf("attached session B lost: ok=%v err=%v", ok, err)
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Session == "b-session" {
			t.Fatalf("reconciliation touched B: %+v", e)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "hook.log")); err == nil && strings.Contains(string(data), "failed") {
		t.Fatalf("reconciliation failed on B: %s", data)
	}
}

// Without a complete process table read after the panes, reconciliation is
// skipped rather than judging an attachment by an older table.
func TestReconciliationSkipsWithoutFreshProcessEvidence(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := session.Record{Agent: proc.Claude, ID: "b-session", Main: adapter.Idle,
		Host: session.Host{PID: 200, Start: "s-b"}}
	if err := store.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	previousProcesses, previousPanes := loadProcesses, loadPanes
	t.Cleanup(func() { loadProcesses, loadPanes = previousProcesses, previousPanes })
	calls := 0
	loadProcesses = func(_ context.Context, w *attach.World) error {
		calls++
		if calls > 1 {
			return errors.New("process table unreadable")
		}
		w.Proc = proc.NewTable([]proc.Process{{PID: 100, PPID: 1, Comm: "sh", Args: "sh"}})
		return nil
	}
	loadPanes = func(_ context.Context, w *attach.World) error {
		w.Panes = []tmux.Pane{{Server: "r1", ID: "%1", PID: 100, Command: "claude", Options: map[string]string{
			"agent": "claude", "session": "b-session", "pid": "200", "start": "s-b",
			"evidence": "process", "state": "idle",
		}}}
		return nil
	}

	payload := `{"session_id":"a-session","hook_event_name":"UserPromptSubmit","prompt_id":"turn"}`
	if err := RunContext(ctx, Input{Agent: proc.Claude, Payload: strings.NewReader(payload), Log: log, PID: 0}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("process table read %d times, want a refresh after the panes", calls)
	}
	if data, err := os.ReadFile(filepath.Join(root, "hook.log")); err == nil {
		t.Fatalf("skipped reconciliation was logged: %s", data)
	}
}
