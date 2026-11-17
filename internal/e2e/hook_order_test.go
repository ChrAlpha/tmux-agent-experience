package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/sockettest"
)

// A tmux server that accepts and never answers used to hold a hook before it
// wrote its record. The record and its event now come first, and the hook
// still finishes within hook.Deadline.
func TestHangingTmuxCannotKeepAHookFromRecording(t *testing.T) {
	sock := sockettest.Dir(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", sock)
	t.Setenv("TMAG_STATE_DIR", state)
	t.Setenv("TMAG_AUTH_DIR", filepath.Join(dir, "auth"))

	socketDir := filepath.Join(sock, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(socketDir, "hang"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	var accepting sync.WaitGroup
	accepting.Add(1)
	go func() {
		defer accepting.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		accepting.Wait()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	const sid = "hang-00000000-0000-7000-8000-000000000001"
	h := &harness{t: t, dir: dir, state: state}
	payload := fmt.Sprintf(`{"session_id":%q,"hook_event_name":"UserPromptSubmit","prompt_id":"p1"}`, sid)
	started := time.Now()
	r := h.tmag(payload, "hook", "claude")
	elapsed := time.Since(started)
	if r.code != 0 || r.out != "" {
		t.Fatalf("hook: %d %q", r.code, r.out)
	}
	if elapsed > hook.Deadline+2*time.Second {
		t.Fatalf("hook took %s, past its deadline", elapsed)
	}

	rec, ok, err := (session.Store{Dir: state}).Get(context.Background(), proc.Claude, sid)
	if err != nil || !ok || rec.State() != adapter.Working {
		t.Fatalf("record = %+v, %v, %v", rec, ok, err)
	}
	events, _, err := eventlog.Open(state).Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		found = found || (e.Session == sid && e.Event == "UserPromptSubmit" && e.State == adapter.Working)
	}
	if !found {
		t.Fatalf("events = %+v", events)
	}
}
