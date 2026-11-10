package inbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// serve listens at a fresh socket and runs handle for every connection, in
// the order the connections arrive.
func serve(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "inbox") // socket paths are short
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
				defer conn.Close()
				handle(conn)
			}()
		}
	}()
	return sock
}

func dial(t *testing.T, sock string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestExtensionFraming(t *testing.T) {
	lines := make(chan string, 1)
	sock := serve(t, func(conn net.Conn) {
		line, _ := bufio.NewReader(conn).ReadString('\n')
		lines <- line
		_, _ = io.WriteString(conn, "{\"ok\":true}\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dial(t, sock).Extension(ctx, "hello\nworld", "s1"); err != nil {
		t.Fatal(err)
	}
	line := <-lines
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("one line expected, got %q", line)
	}
	var req struct {
		Send    string `json:"send"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil || req.Send != "hello\nworld" || req.Session != "s1" {
		t.Fatalf("request %q: %q, %v", line, req.Send, err)
	}
}

func TestExtensionSaysWhenItTookNothing(t *testing.T) {
	sock := serve(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = io.WriteString(conn, "{\"ok\":false,\"error\":\"no active turn\"}\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := dial(t, sock).Extension(ctx, "hello", "s1")
	if !errors.Is(err, ErrNotTaken) || !strings.Contains(err.Error(), "no active turn") {
		t.Fatalf("Extension = %v", err)
	}
}

// A process that serves another session refuses the request, and nothing was
// delivered.
func TestExtensionRefusesAnotherSession(t *testing.T) {
	sock := serve(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = io.WriteString(conn, "{\"ok\":false,\"refused\":\"changed\",\"error\":\"this agent now serves s2\"}\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := dial(t, sock).Extension(ctx, "hello", "s1")
	var refused *Refusal
	if !errors.As(err, &refused) || refused.Reason != "changed" || refused.Detail != "this agent now serves s2" {
		t.Fatalf("Extension = %v", err)
	}
}

func TestExtensionWithoutAnAnswerIsUnknown(t *testing.T) {
	// The extension protocol answers every request; a missing answer proves
	// nothing about the message, so it is not ErrNotTaken.
	sock := serve(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := dial(t, sock).Extension(ctx, "hello", "s1")
	if err == nil || errors.Is(err, ErrNotTaken) {
		t.Fatalf("Extension = %v", err)
	}
}

func TestExtensionHonoursItsContext(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	sock := serve(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		<-done // never answers, until the test ends
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := dial(t, sock).Extension(ctx, "hello", "s1")
	if err == nil || errors.Is(err, ErrNotTaken) || time.Since(start) > 2*time.Second {
		t.Fatalf("Extension = %v after %s", err, time.Since(start))
	}
}

func TestClaudeFraming(t *testing.T) {
	lines := make(chan string, 1)
	eofs := make(chan error, 1)
	sock := serve(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		lines <- line
		_, err := r.ReadByte() // the sender's half-close
		eofs <- err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dial(t, sock).Claude(ctx, "tmag", "hello"); err != nil {
		t.Fatal(err)
	}
	// The framing, exactly: one user message whose content is the envelope,
	// and no auth line.
	if got, want := <-lines, `{"type":"user","message":{"role":"user","content":"<cross-session-message from-name=\"tmag\">\nhello\n</cross-session-message>"}}`+"\n"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	if err := <-eofs; !errors.Is(err, io.EOF) {
		t.Fatalf("the sender did not close its write half: %v", err)
	}
}

// Claude Code closes its side once it read the frame and saw tmag's EOF.
// Without that close nothing proves the inbox read anything.
func TestClaudeRequiresEOF(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	sock := serve(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		<-done // keeps the connection open, sending nothing
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := dial(t, sock).Claude(ctx, "tmag", "hello"); err == nil {
		t.Fatal("Claude returned without EOF")
	}
}

func TestClaudeLineEscapesTheText(t *testing.T) {
	line, err := claudeLine("tmag", "a \"quoted\"\nline")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &m); err != nil || m.Type != "user" || m.Message.Role != "user" || m.Message.Content != "<cross-session-message from-name=\"tmag\">\na \"quoted\"\nline\n</cross-session-message>" {
		t.Fatalf("line %q decodes to %+v, %v", line, m, err)
	}
	if strings.Count(string(line), "\n") != 1 {
		t.Fatalf("line %q is not one line", line)
	}
}

// A sender's name is cleaned as Claude Code's builder does: quotes, angle
// brackets, CR and LF go; & and backslashes stay as they are.
func TestCleanName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`a"b<c>&d`, "abc&d"},
		{"a\r\nb", "ab"},
		{"a<b>c", "abc"},
		{`a\b`, `a\b`},
		{"", ""},
	} {
		if got := cleanName(c.in); got != c.want {
			t.Errorf("cleanName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	line, err := claudeLine(`a"b<c>&d`, "hi")
	if err != nil || !strings.Contains(string(line), `from-name=\"abc&d\"`) {
		t.Fatalf("the envelope does not carry the cleaned name: %q, %v", line, err)
	}
}

func TestServesTheRecordedProcess(t *testing.T) {
	sock := serve(t, func(net.Conn) {})
	c := dial(t, sock)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tb, err := proc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	self, ok := tb.Get(os.Getpid())
	if !ok {
		t.Fatal("this process is not in its own process table")
	}
	if self.Start == "" {
		t.Fatal("this process has no start time")
	}
	if !c.Serves(ctx, session.Inbox{Path: sock, PID: os.Getpid(), Start: self.Start}) {
		t.Fatal("the listening process does not serve its own socket")
	}
	for _, in := range []session.Inbox{
		{Path: sock, PID: os.Getpid() + 1, Start: self.Start},
		{Path: sock, PID: os.Getpid(), Start: "0"},
		{Path: sock, PID: os.Getpid()},
		{Path: sock, Start: self.Start},
	} {
		if c.Serves(ctx, in) {
			t.Errorf("%d/%q serves %s", in.PID, in.Start, sock)
		}
	}
}

func TestDialFailsWithoutAListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c, err := Dial(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		c.Close()
		t.Fatal("Dial succeeded")
	}
}
