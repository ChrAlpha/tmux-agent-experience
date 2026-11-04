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
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc/svctest"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// daemonHolding serves a Codex control socket that replays the given
// command approvals of thread "th", turn "tu", by item id, to a client that
// joins.
func daemonHolding(t *testing.T, items ...string) *codexsvc.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "tmag-q-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		c, err := svctest.Accept(conn)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			m, err := c.Read()
			if err != nil {
				return
			}
			_ = c.Reply(m.ID, map[string]string{})
			if m.Method == "thread/resume" {
				for i, item := range items {
					_ = c.Send(map[string]any{"id": i + 1, "method": codexsvc.CommandApproval, "params": map[string]string{"threadId": "th", "turnId": "tu", "itemId": item}})
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	c, err := codexsvc.Dial(ctx, filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// requestAfter is the request a session names after trs, for the prompt
// of tool.
func requestAfter(t *testing.T, tool string, trs ...adapter.Transition) session.Request {
	t.Helper()
	rec := session.Record{Agent: proc.Codex, ID: "th", Main: adapter.Working, Turn: "tu"}
	for _, tr := range trs {
		rec, _ = session.Apply(rec, tr, time.Now())
	}
	for _, q := range rec.Requests {
		if q.Prompt.Tool == tool {
			return q
		}
	}
	t.Fatalf("no request for %s: %+v", tool, rec.Requests)
	return session.Request{}
}

func started(tool, call string) adapter.Transition {
	return adapter.Transition{Action: adapter.Start, Turn: "tu", Call: adapter.Call{Tool: tool, ID: call}}
}

func asks(tool, id string) adapter.Transition {
	return adapter.Transition{Action: adapter.Ask, Turn: "tu", Prompt: adapter.Prompt{Tool: tool}, Request: id}
}

func TestApprovalIsTheRequestsCall(t *testing.T) {
	ctx := context.Background()
	s := Session{Agent: proc.Codex, ID: "th"}

	// Bash's request stays open while its approved command runs; write_stdin
	// asks through a command approval too. Only the call tells them apart.
	stale := requestAfter(t, "Bash", started("Bash", "a"), asks("Bash", "r1"), started("write_stdin", "b"), asks("write_stdin", "r2"))
	if _, m, err := pendingApproval(ctx, daemonHolding(t, "b"), s, stale); !isRefused(err, RefuseChanged) {
		t.Fatalf("the Bash request took write_stdin's approval: %s %v", m.Params, err)
	}
	asking := requestAfter(t, "write_stdin", started("Bash", "a"), asks("Bash", "r1"), started("write_stdin", "b"), asks("write_stdin", "r2"))
	if _, m, err := pendingApproval(ctx, daemonHolding(t, "a2", "b"), s, asking); err != nil || string(m.ID) != "2" {
		t.Fatalf("write_stdin's approval: %s %v", m.ID, err)
	}

	// Two Bash calls running when it asks: either may be asking.
	either := requestAfter(t, "Bash", started("Bash", "a"), started("Bash", "b"), asks("Bash", "r1"))
	if _, _, err := pendingApproval(ctx, daemonHolding(t, "a", "b"), s, either); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("a request about either call: %v", err)
	}
}

func isRefused(err error, reason string) bool {
	var r *RefusedError
	return errors.As(err, &r) && r.Reason == reason
}
