// Package svctest is the server end of the Codex control socket's framing,
// for tests and the fake agent: it accepts a WebSocket upgrade and then
// exchanges JSON-RPC messages.
package svctest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
)

// Conn is one accepted client connection.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex // orders writes
}

// Accept completes the client's WebSocket upgrade.
func Accept(conn net.Conn) (*Conn, error) {
	r := bufio.NewReader(conn)
	req, err := http.ReadRequest(r)
	if err != nil {
		return nil, err
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if req.Header.Get("Upgrade") != "websocket" || key == "" {
		_, _ = fmt.Fprint(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return nil, fmt.Errorf("not a websocket upgrade")
	}
	_, err = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", codexsvc.AcceptKey(key))
	if err != nil {
		return nil, err
	}
	return &Conn{conn: conn, r: r}, nil
}

// Message is one message from the client: a request (ID, Method, Params) or
// a response to a request of the server's (ID, Result).
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Read returns the next message.
func (c *Conn) Read() (Message, error) {
	b, err := codexsvc.ReadMessage(c.r, c, false)
	if err != nil {
		return Message{}, err
	}
	var m Message
	return m, json.Unmarshal(b, &m)
}

// Send sends one message.
func (c *Conn) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return codexsvc.WriteFrame(c, codexsvc.OpText, b, false)
}

// Reply answers request id with result.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	return c.Send(map[string]any{"id": id, "result": result})
}

// Fail answers request id with an error.
func (c *Conn) Fail(id json.RawMessage, message string) error {
	return c.Send(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": message}})
}

// Close ends the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Write writes whole frames to the connection, one at a time, for Send and
// for the pongs ReadMessage sends.
func (c *Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Write(b)
}
