// Package inbox hands a message to an agent through the socket the agent
// itself serves for messages: Claude Code's cross-session inbox, or tmag's
// extension in Pi and oh-my-pi. tmag writes the message and keeps none of it.
//
// A connection proves its peer: the process at the other end (SO_PEERCRED,
// LOCAL_PEERPID on macOS) must be the process that named the socket, still
// running as it did. Only the extension's answer says whether it took
// anything. Claude Code answers nothing on the connection: tmag closes its
// write half, and the EOF Claude Code returns once it read the frame shows
// the inbox took it, and nothing more.
package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// ErrNotTaken reports an agent that answered it took nothing: the message
// may be sent again.
var ErrNotTaken = errors.New("the agent took nothing")

// Refusal reports an agent that refused a request for a session it no longer
// serves. Nothing was delivered.
type Refusal struct {
	Reason string
	Detail string
}

func (e *Refusal) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Reason
}

// Conn is one connection to an agent's inbox socket.
type Conn struct {
	conn *net.UnixConn
	// PeerPID is the process that serves the socket.
	PeerPID int
}

// Dial connects to the inbox socket at path and learns which process serves
// it.
func Dial(ctx context.Context, path string) (*Conn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return nil, fmt.Errorf("%s is not a Unix socket", path)
	}
	pid, err := proc.PeerPID(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("who serves %s: %w", path, err)
	}
	return &Conn{conn: conn, PeerPID: pid}, nil
}

// Close ends the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Serves reports whether the connection reaches the process an inbox
// records: the peer is that process, and it still runs as it did. A socket
// another process was given the path of, or a pid the kernel reused, is no
// proof.
func (c *Conn) Serves(ctx context.Context, in session.Inbox) bool {
	return in.PID > 0 && in.Start != "" && c.PeerPID == in.PID && proc.RunningContext(ctx, in.PID, in.Start)
}

// Claude writes text as one message line on Claude Code's cross-session
// inbox, under an envelope naming the sender, closes the write half and waits
// for Claude Code to close its side. Claude Code answers nothing on this
// connection: its EOF once it sees tmag's proves the inbox read the frame, and
// nothing more.
func (c *Conn) Claude(ctx context.Context, name, text string) error {
	return c.within(ctx, func() error {
		line, err := claudeLine(name, text)
		if err != nil {
			return err
		}
		if err := c.write(ctx, line); err != nil {
			return err
		}
		if err := c.conn.CloseWrite(); err != nil {
			return err
		}
		_ = c.conn.SetReadDeadline(deadline(ctx))
		_, err = io.Copy(io.Discard, c.conn)
		return err
	})
}

// claudeLine is the one line Claude Code's inbox takes: a user message whose
// content is the canonical envelope naming the sender, then the text. The auth
// line its docs describe is optional and tmag sends none; the name carries no
// permission class.
func claudeLine(name, text string) ([]byte, error) {
	m := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	m.Message.Role = "user"
	m.Message.Content = `<cross-session-message from-name="` + cleanName(name) + `">` + "\n" + text + "\n</cross-session-message>"
	// Claude Code's own builder writes the tags as they are, not as JSON's
	// HTML escapes.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cleanName removes what Claude Code's own builder removes from a sender's
// name: double quotes, angle brackets, CR and LF. Entities and backslashes
// would corrupt it, and & is left as it is.
func cleanName(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '<', '>', '\r', '\n':
			return -1
		}
		return r
	}, name)
}

// Extension writes text as one message request for session to tmag's
// extension in Pi or oh-my-pi and reads its answer. The extension answers
// after it handed the text to the session it still serves, or refuses a
// session it no longer serves, or says it took nothing.
func (c *Conn) Extension(ctx context.Context, text, session string) error {
	return c.within(ctx, func() error {
		line, err := json.Marshal(struct {
			Send    string `json:"send"`
			Session string `json:"session"`
		}{Send: text, Session: session})
		if err != nil {
			return err
		}
		if err := c.write(ctx, append(line, '\n')); err != nil {
			return err
		}
		_ = c.conn.SetReadDeadline(deadline(ctx))
		var r struct {
			OK      *bool  `json:"ok"`
			Refused string `json:"refused"`
			Error   string `json:"error"`
		}
		if err := json.NewDecoder(c.conn).Decode(&r); err != nil {
			return fmt.Errorf("the extension answered nothing: %w", err)
		}
		switch {
		case r.OK == nil:
			return errors.New("the extension answered no ok")
		case !*r.OK:
			if r.Refused != "" {
				return &Refusal{Reason: r.Refused, Detail: r.Error}
			}
			if r.Error == "" {
				return ErrNotTaken
			}
			return fmt.Errorf("%w: %s", ErrNotTaken, r.Error)
		}
		return nil
	})
}

// within runs one exchange, ending its I/O when ctx ends: a canceled send
// does not wait for its deadline.
func (c *Conn) within(ctx context.Context, fn func() error) error {
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	return fn()
}

// write sends line within ctx.
func (c *Conn) write(ctx context.Context, line []byte) error {
	_ = c.conn.SetWriteDeadline(deadline(ctx))
	_, err := c.conn.Write(line)
	return err
}

// deadline is ctx's deadline, or the zero time for none.
func deadline(ctx context.Context) time.Time {
	at, _ := ctx.Deadline()
	return at
}
