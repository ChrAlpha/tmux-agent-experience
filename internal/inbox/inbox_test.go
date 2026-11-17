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
	"github.com/ChrAlpha/tmux-agent-experience/internal/sockettest"
)

// serve listens at a fresh socket and runs handle for every connection, in
// the order the connections arrive.
func serve(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	sock := filepath.Join(sockettest.Dir(t), "s.sock")
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

func TestExtensionInterruptFraming(t *testing.T) {
	lines := make(chan string, 1)
	sock := serve(t, func(conn net.Conn) {
		line, _ := bufio.NewReader(conn).ReadString('\n')
		lines <- line
		_, _ = io.WriteString(conn, "{\"ok\":true}\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dial(t, sock).Interrupt(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Interrupt bool   `json:"interrupt"`
		Session   string `json:"session"`
	}
	if err := json.Unmarshal([]byte(<-lines), &req); err != nil || !req.Interrupt || req.Session != "s1" {
		t.Fatalf("request = %+v, %v", req, err)
	}
}

// An idle session's refusal keeps its own reason, so an interrupt can be
// told from one that reached another session.
func TestExtensionInterruptRefusal(t *testing.T) {
	sock := serve(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = io.WriteString(conn, "{\"ok\":false,\"refused\":\"idle\",\"error\":\"this agent runs no turn to interrupt\"}\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := dial(t, sock).Interrupt(ctx, "s1")
	var refused *Refusal
	if !errors.As(err, &refused) || refused.Reason != "idle" || refused.Detail != "this agent runs no turn to interrupt" {
		t.Fatalf("Interrupt = %v", err)
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
	if err := dial(t, sock).Claude(ctx, ClaudeMessage{Name: "tmag", Mode: "prompting", Session: "s1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	// The framing, exactly: one user message whose content is the envelope,
	// the receiver's session, and no auth line.
	if got, want := <-lines, `{"type":"user","message":{"role":"user","content":"<cross-session-message from-name=\"tmag\" from-mode=\"prompting\">\nhello\n</cross-session-message>"},"session_id":"s1"}`+"\n"; got != want {
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
	if err := dial(t, sock).Claude(ctx, ClaudeMessage{Name: "tmag", Text: "hello"}); err == nil {
		t.Fatal("Claude returned without EOF")
	}
}

func TestClaudeLineEscapesTheText(t *testing.T) {
	line, err := claudeLine(ClaudeMessage{Name: "tmag", Session: "s1", Text: "a \"quoted\"\nline"})
	if err != nil {
		t.Fatal(err)
	}
	// A line without a class attests none: the attribute is absent, not empty.
	want := `{"type":"user","message":{"role":"user","content":"<cross-session-message from-name=\"tmag\">\na \"quoted\"\nline\n</cross-session-message>"},"session_id":"s1"}` + "\n"
	if string(line) != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
}

// Only the classes Claude Code accepts reach its envelope: bypass and
// prompting; a raw mode or anything else attests nothing.
func TestClaudeLineKeepsOnlyTheClass(t *testing.T) {
	for mode, want := range map[string]bool{
		"bypass":            true,
		"prompting":         true,
		"bypassPermissions": false,
		"default":           false,
		"":                  false,
	} {
		line, err := claudeLine(ClaudeMessage{Name: "tmag", Mode: mode, Session: "s1", Text: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(string(line), `from-mode=\"`)
		if got != want {
			t.Errorf("mode %q wrote an attribute: %v, want %v: %s", mode, got, want, line)
		}
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
	line, err := claudeLine(ClaudeMessage{Name: `a"b<c>&d`, Text: "hi"})
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
