// Package codexsvc is a client of the Codex app-server daemon's control
// socket, for one tmag command at a time: tmag connects to the agent's own
// service for as long as a verb takes, and runs nothing of its own.
//
// The socket speaks JSON-RPC over WebSocket. Each request is one object
// without a "jsonrpc" field; the server sends notifications and requests of
// its own (approvals) between responses.
package codexsvc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// Socket is the daemon's control socket: $CODEX_HOME (default ~/.codex),
// app-server-control/app-server-control.sock, which Codex makes a link to the
// socket it listens on.
func Socket() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			h = os.TempDir()
		}
		home = filepath.Join(h, ".codex")
	}
	return filepath.Join(home, "app-server-control", "app-server-control.sock")
}

// Message is one message from the server: a response (ID with Result or
// Error), a notification (Method, Params), or a request of the server's own
// (ID, Method, Params).
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Request reports whether m is a request of the server's.
func (m Message) Request() bool { return len(m.ID) > 0 && m.Method != "" }

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("codex app-server: %s (%d)", e.Message, e.Code) }

// Client is one connection to the daemon.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	// PeerPID is the process that serves the socket.
	PeerPID int
	next    int64
	inbox   []Message
	// resolved are the server requests the daemon reported resolved, by
	// their ids, whoever read the report.
	resolved map[string]bool
}

// Dial connects to the socket, learns which process serves it, and
// initializes the connection.
func Dial(ctx context.Context, socket string) (*Client, error) {
	// Codex keeps the socket itself under /tmp and links it from CODEX_HOME,
	// whose path may be too long for a socket address.
	target := socket
	if real, err := filepath.EvalSymlinks(socket); err == nil {
		target = real
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", target)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn)}
	if c.PeerPID, err = peerPID(conn.(*net.UnixConn)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("who serves %s: %w", socket, err)
	}
	err = c.within(ctx, func() error { return handshake(conn, c.r) })
	if err == nil {
		err = c.Call(ctx, "initialize", map[string]any{
			"clientInfo":   map[string]string{"name": "tmag", "title": "tmag", "version": version.Version},
			"capabilities": map[string]bool{"experimentalApi": false},
		}, nil)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// Close ends the connection.
func (c *Client) Close() error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	_ = WriteFrame(c.conn, OpClose, nil, true)
	return c.conn.Close()
}

// within runs fn with the connection's deadline set from ctx.
func (c *Client) within(ctx context.Context, fn func() error) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	err := fn()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(c.conn, OpText, b, true)
}

func (c *Client) read() (Message, error) {
	b, err := ReadMessage(c.r, c.conn, true)
	if err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, fmt.Errorf("codex app-server sent %d bytes that are not JSON-RPC: %w", len(b), err)
	}
	return m, nil
}

// Call sends a request and waits for its response, decoding the result into
// result unless it is nil. Messages that arrive meanwhile wait for Next.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	c.next++
	id := c.next
	return c.within(ctx, func() error {
		if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		want := strconv.FormatInt(id, 10)
		for {
			m, err := c.read()
			if err != nil {
				return err
			}
			if m.Method != "" || string(m.ID) != want {
				c.inbox = append(c.inbox, m)
				continue
			}
			if m.Error != nil {
				return m.Error
			}
			if result == nil {
				return nil
			}
			return json.Unmarshal(m.Result, result)
		}
	})
}

// Next returns the next notification or server request.
func (c *Client) Next(ctx context.Context) (Message, error) {
	if len(c.inbox) > 0 {
		m := c.inbox[0]
		c.inbox = c.inbox[1:]
		return c.seen(m), nil
	}
	var m Message
	err := c.within(ctx, func() error {
		var err error
		m, err = c.read()
		return err
	})
	return c.seen(m), err
}

// resolvedMethod is the notification that a server request is over.
const resolvedMethod = "serverRequest/resolved"

// seen notes m if it reports a server request resolved.
func (c *Client) seen(m Message) Message {
	if m.Method != resolvedMethod {
		return m
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(m.Params, &p) == nil && len(p.RequestID) > 0 {
		if c.resolved == nil {
			c.resolved = map[string]bool{}
		}
		c.resolved[string(p.RequestID)] = true
	}
	return m
}

// Respond answers a request of the server's.
func (c *Client) Respond(ctx context.Context, id json.RawMessage, result any) error {
	return c.within(ctx, func() error {
		return c.send(map[string]any{"id": id, "result": result})
	})
}

// Thread statuses.
const (
	StatusIdle      = "idle"
	StatusActive    = "active"
	StatusNotLoaded = "notLoaded"
)

// ThreadStatus returns a thread's status as the daemon has it, without
// loading or joining the thread.
func (c *Client) ThreadStatus(ctx context.Context, thread string) (string, error) {
	var out struct {
		Thread struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false}, &out); err != nil {
		return "", err
	}
	if out.Thread.Status.Type == "" {
		return "", errors.New("codex app-server: thread/read returned no status")
	}
	return out.Thread.Status.Type, nil
}

// StartTurn submits text to a thread and returns the turn it went to. On a
// busy thread the daemon steers the active turn instead of starting one.
func (c *Client) StartTurn(ctx context.Context, thread, text string) (string, error) {
	var out struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	params := map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": text}}}
	if err := c.Call(ctx, "turn/start", params, &out); err != nil {
		return "", err
	}
	return out.Turn.ID, nil
}

// InterruptTurn interrupts a thread's active turn. The daemon answers once
// the turn has stopped.
func (c *Client) InterruptTurn(ctx context.Context, thread, turn string) error {
	return c.Call(ctx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": turn}, nil)
}

// Join subscribes to a loaded thread, without overrides (which would restart
// an idle thread nobody else watches), and returns the requests of the
// daemon's still waiting for an answer, which it replays to a newcomer. A
// request resolved while the replay lasts is not among them.
func (c *Client) Join(ctx context.Context, thread string) ([]Message, error) {
	if err := c.Call(ctx, "thread/resume", map[string]any{"threadId": thread, "excludeTurns": true}, nil); err != nil {
		return nil, err
	}
	// The replay follows the response at once; a short quiet ends it.
	var out []Message
	for {
		quiet, cancel := context.WithTimeout(ctx, replayQuiet)
		m, err := c.Next(quiet)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return slices.DeleteFunc(out, func(r Message) bool { return c.resolved[string(r.ID)] }), nil
		}
		if m.Request() {
			out = append(out, m)
		}
	}
}

// replayQuiet is how long Join waits for more replayed requests.
const replayQuiet = 300 * time.Millisecond

// Leave unsubscribes from a thread.
func (c *Client) Leave(ctx context.Context, thread string) error {
	return c.Call(ctx, "thread/unsubscribe", map[string]string{"threadId": thread}, nil)
}

// AwaitResolved waits until the daemon reports request id resolved, by
// this client's answer or another's: responses get no acknowledgement.
func (c *Client) AwaitResolved(ctx context.Context, id json.RawMessage) error {
	for !c.resolved[string(id)] {
		if _, err := c.Next(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Approval requests tmag can answer, with the decisions they take.
const (
	CommandApproval = "item/commandExecution/requestApproval"
	FileApproval    = "item/fileChange/requestApproval"
)

// Decisions on an approval.
const (
	Accept  = "accept"
	Decline = "decline"
)
