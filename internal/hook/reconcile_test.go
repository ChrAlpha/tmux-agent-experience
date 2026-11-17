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
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// The pane a session B attached to between the hook's lineage read and its
// pane listing stays attached: reconciliation reads a complete process table
// after the panes, so the fresher attachment has its client.
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
	listed := false
	sources := Sources{
		Lineage: func(context.Context, int) (*proc.Table, error) { return proc.NewTable(nil), nil },
		Panes: func(context.Context) ([]tmux.Pane, error) {
			listed = true
			return panes, nil
		},
		Processes: func(context.Context) (*proc.Table, error) {
			if !listed {
				t.Fatal("process table read before the panes")
			}
			return proc.NewTable([]proc.Process{
				{PID: 100, PPID: 1, Comm: "sh", Args: "sh"},
				{PID: 200, PPID: 100, Comm: "claude", Args: "claude", Start: "s-b"},
			}), nil
		},
	}

	payload := `{"session_id":"a-session","hook_event_name":"SessionStart","source":"startup"}`
	if err := Run(ctx, Input{Agent: proc.Claude, Payload: strings.NewReader(payload), Log: log, PID: 0, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, proc.Claude, "b-session"); !ok || err != nil {
		t.Fatalf("attached session B lost: ok=%v err=%v", ok, err)
	}
	events, err := log.Since(ctx, 0)
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
	listed := false
	sources := Sources{
		Lineage: func(context.Context, int) (*proc.Table, error) { return proc.NewTable(nil), nil },
		Panes: func(context.Context) ([]tmux.Pane, error) {
			listed = true
			return []tmux.Pane{{Server: "r1", ID: "%1", PID: 100, Command: "claude", Options: map[string]string{
				"agent": "claude", "session": "b-session", "pid": "200", "start": "s-b",
				"evidence": "process", "state": "idle",
			}}}, nil
		},
		Processes: func(context.Context) (*proc.Table, error) {
			if !listed {
				t.Fatal("process table read before the panes")
			}
			return nil, errors.New("process table unreadable")
		},
	}

	payload := `{"session_id":"a-session","hook_event_name":"SessionStart","source":"startup"}`
	if err := Run(ctx, Input{Agent: proc.Claude, Payload: strings.NewReader(payload), Log: log, PID: 0, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "hook.log")); err == nil {
		t.Fatalf("skipped reconciliation was logged: %s", data)
	}
}

// Tool hooks do not reconcile: a session whose host is gone stays until a
// session start or end, or a read path, reconciles it.
func TestToolHookDoesNotReconcile(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gone := session.Record{Agent: proc.Claude, ID: "gone-session", Main: adapter.Idle,
		Host: session.Host{PID: 200, Start: "s-gone"}}
	if err := store.Put(ctx, gone); err != nil {
		t.Fatal(err)
	}
	sources := Sources{
		Lineage:   func(context.Context, int) (*proc.Table, error) { return proc.NewTable(nil), nil },
		Panes:     func(context.Context) ([]tmux.Pane, error) { return nil, nil },
		Processes: func(context.Context) (*proc.Table, error) { return proc.NewTable(nil), nil },
	}
	payload := `{"session_id":"a-session","hook_event_name":"UserPromptSubmit","prompt_id":"turn"}`
	if err := Run(ctx, Input{Agent: proc.Claude, Payload: strings.NewReader(payload), Log: log, PID: 0, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, proc.Claude, "gone-session"); !ok || err != nil {
		t.Fatalf("a tool hook reconciled: ok=%v err=%v", ok, err)
	}
}
