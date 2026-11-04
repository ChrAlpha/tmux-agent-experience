package codexsvc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc/svctest"
)

// serve runs handle for every connection to a fresh socket.
func serve(t *testing.T, handle func(c *svctest.Conn, m svctest.Message)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "svc") // socket paths are short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c, err := svctest.Accept(conn)
				if err != nil {
					conn.Close()
					return
				}
				defer c.Close()
				for {
					m, err := c.Read()
					if err != nil {
						return
					}
					if m.Method == "initialize" {
						_ = c.Reply(m.ID, map[string]string{"userAgent": "fake"})
						continue
					}
					handle(c, m)
				}
			}()
		}
	}()
	return sock
}

func TestClient(t *testing.T) {
	big := strings.Repeat("x", 70000) // needs a 64-bit frame length
	sock := serve(t, func(c *svctest.Conn, m svctest.Message) {
		switch m.Method {
		case "thread/read":
			// A notification and a request of the server's come first.
			_ = c.Send(map[string]any{"method": "thread/status/changed", "params": map[string]string{"threadId": "t1"}})
			_ = c.Send(map[string]any{"id": 0, "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "t1"}})
			_ = c.Reply(m.ID, map[string]any{"thread": map[string]any{"id": "t1", "status": map[string]string{"type": "idle"}, "preview": big}})
		case "turn/start":
			var p struct {
				ThreadID string `json:"threadId"`
				Input    []struct{ Type, Text string }
			}
			_ = json.Unmarshal(m.Params, &p)
			_ = c.Reply(m.ID, map[string]any{"turn": map[string]string{"id": "turn-" + p.ThreadID + "-" + p.Input[0].Text}})
		case "turn/interrupt":
			_ = c.Fail(m.ID, "no active turn to interrupt")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := codexsvc.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.PeerPID != os.Getpid() {
		t.Fatalf("peer pid = %d, want %d", c.PeerPID, os.Getpid())
	}
	if st, err := c.ThreadStatus(ctx, "t1"); err != nil || st != codexsvc.StatusIdle {
		t.Fatalf("ThreadStatus = %q, %v", st, err)
	}
	if m, err := c.Next(ctx); err != nil || m.Method != "thread/status/changed" || m.Request() {
		t.Fatalf("first queued message = %+v, %v", m, err)
	}
	if m, err := c.Next(ctx); err != nil || !m.Request() || string(m.ID) != "0" {
		t.Fatalf("second queued message = %+v, %v", m, err)
	}
	if id, err := c.StartTurn(ctx, "t1", "hi"); err != nil || id != "turn-t1-hi" {
		t.Fatalf("StartTurn = %q, %v", id, err)
	}
	var rpc *codexsvc.RPCError
	if err := c.InterruptTurn(ctx, "t1", "x"); err == nil || !strings.Contains(err.Error(), "no active turn") {
		t.Fatalf("InterruptTurn = %v", err)
	} else if !errors.As(err, &rpc) {
		t.Fatalf("error %T is not an RPC error", err)
	}
}

func TestClientGivesUpWithItsContext(t *testing.T) {
	sock := serve(t, func(*svctest.Conn, svctest.Message) {}) // never answers
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := codexsvc.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	short, cancelShort := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelShort()
	start := time.Now()
	if _, err := c.ThreadStatus(short, "t1"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("ThreadStatus without an answer: %v after %s", err, time.Since(start))
	}
}

func TestJoinLeavesOutWhatResolvesDuringTheReplay(t *testing.T) {
	sock := serve(t, func(c *svctest.Conn, m svctest.Message) {
		if m.Method != "thread/resume" {
			return
		}
		_ = c.Reply(m.ID, map[string]any{})
		// Two approvals are replayed; someone answers the first meanwhile.
		_ = c.Send(map[string]any{"id": 4, "method": codexsvc.CommandApproval, "params": map[string]string{"threadId": "t1"}})
		_ = c.Send(map[string]any{"id": 5, "method": codexsvc.FileApproval, "params": map[string]string{"threadId": "t1"}})
		_ = c.Send(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": "t1", "requestId": 4}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := codexsvc.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pending, err := c.Join(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || string(pending[0].ID) != "5" {
		t.Fatalf("Join = %+v, want only request 5", pending)
	}
	// The report Join read still counts for a wait on it.
	short, cancelShort := context.WithTimeout(ctx, time.Second)
	defer cancelShort()
	if err := c.AwaitResolved(short, json.RawMessage("4")); err != nil {
		t.Fatalf("AwaitResolved(4) = %v", err)
	}
}
