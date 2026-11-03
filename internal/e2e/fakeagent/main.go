// Command fakeagent impersonates an interactive coding agent in end-to-end
// tests. Install it as "claude" or "codex"; the name selects the agent.
//
// Environment:
//
//	FAKE_SESSION  session id reported to hooks
//	FAKE_OUT      file that receives every submitted line
//	FAKE_TITLE    terminal title to set on start (OSC 0)
//	FAKE_HOOKS    "1" to run `tmag hook` itself, as in-process agents do
//	FAKE_TURN_MS  how long a turn lasts (default 200)
//	FAKE_KEYS     "1" to read keys as a TUI does: Escape interrupts a turn
//	FAKE_ANSWER   "1" to open "block" (Bash) and "ask" (a question) as Claude
//	              Code does: the answer hook waits beside the dialog, and the
//	              next line ("yes" or anything else) answers it in the terminal
//	TMAG_BIN      tmag binary
//
// Lines starting with "/" behave like slash commands: no turn, no hooks. The
// line "block" raises a permission prompt that the next line resolves.
// Every line, and how each dialog was answered, goes to FAKE_OUT.
//
// Run as "codex app-server ..." it imitates Codex's shared daemon instead: it
// runs `tmag hook codex` for every JSON payload line on stdin, as its own
// child, outside any pane's agent, and serves the control socket under
// $CODEX_HOME (thread/read, turn/start, turn/interrupt). With FAKE_HOOKS=1 and
// FAKE_SESSION set it first reports that session's start. A turn started
// through the socket lasts FAKE_TURN_MS, default 0.
//
// As a host CLI it answers --version, prints FAKE_PLUGIN_LIST (default "[]")
// for "plugin list --json", and appends any other plugin command to
// FAKE_PLUGIN_LOG.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexsvc/svctest"
)

func main() {
	agent := filepath.Base(os.Args[0])
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "plugin") {
		hostCLI(os.Args[1:])
		return
	}
	if agent == "codex" && len(os.Args) > 1 && os.Args[1] == "app-server" {
		daemon()
		return
	}
	session := os.Getenv("FAKE_SESSION")
	cwd, _ := os.Getwd()
	turnMS, err := strconv.Atoi(os.Getenv("FAKE_TURN_MS"))
	if err != nil {
		turnMS = 200
	}
	if title := os.Getenv("FAKE_TITLE"); title != "" {
		fmt.Printf("\033]0;%s\007", title)
	}
	payloadOf := func(event string, extra map[string]any) []byte {
		payload := map[string]any{"hook_event_name": event, "session_id": session, "cwd": cwd, "transcript_path": filepath.Join(cwd, session+".jsonl")}
		for k, v := range extra {
			payload[k] = v
		}
		b, _ := json.Marshal(payload)
		return b
	}
	hook := func(event string, extra map[string]any) {
		if os.Getenv("FAKE_HOOKS") != "1" {
			return
		}
		cmd := exec.Command(os.Getenv("TMAG_BIN"), "hook", agent)
		cmd.Stdin = strings.NewReader(string(payloadOf(event, extra)))
		_ = cmd.Run()
	}
	turnKey := "turn_id"
	if agent == "claude" {
		turnKey = "prompt_id"
	}

	hook("SessionStart", map[string]any{"source": "startup"})
	fmt.Println("ready")

	record := func(line string) {
		if out := os.Getenv("FAKE_OUT"); out != "" {
			f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				fmt.Fprintln(f, line)
				f.Close()
			}
		}
	}
	// A turn runs while further input arrives: Escape interrupts it, lines
	// wait for it to end.
	var (
		n       int
		turn    string
		ends    <-chan time.Time
		waiting []string
		// dialog is the permission dialog on screen, as Claude Code shows it
		// while its PermissionRequest hooks run.
		dialog  *prompt
		decided = make(chan decision)
	)
	// ask opens a dialog like Claude Code's: the call starts, the answer
	// hook waits beside the dialog, and whichever answers first decides.
	ask := func(tool string, input map[string]any) {
		p := &prompt{tool: tool, call: fmt.Sprintf("u%d", n)}
		hook("PreToolUse", map[string]any{turnKey: turn, "tool_name": tool, "tool_use_id": p.call})
		dialog = p
		payload := payloadOf("PermissionRequest", map[string]any{turnKey: turn, "tool_name": tool, "tool_input": input})
		go func() {
			cmd := exec.Command(os.Getenv("TMAG_BIN"), "answer-hook", agent)
			cmd.Stdin = strings.NewReader(string(payload))
			out, _ := cmd.Output()
			decided <- decision{p, out}
		}()
	}
	// settle ends the dialog's call, run if allowed and refused otherwise,
	// and the turn with it.
	settle := func(allowed bool) {
		if allowed {
			hook("PostToolUse", map[string]any{turnKey: turn, "tool_name": dialog.tool, "tool_use_id": dialog.call})
		} else {
			hook("PostToolBatch", map[string]any{turnKey: turn})
		}
		hook("Stop", map[string]any{turnKey: turn, "last_assistant_message": "done"})
		dialog = nil
	}
	handle := func(line string) {
		n++
		record(line)
		answering := os.Getenv("FAKE_ANSWER") == "1"
		switch {
		case dialog != nil:
			// The person answers the dialog in the terminal.
			record("terminal:" + line)
			settle(line == "yes")
		case strings.HasPrefix(line, "/"):
			// slash commands start no turn
		case line == "block" && answering:
			ask("Bash", map[string]any{"command": "date"})
		case line == "ask" && answering:
			ask("AskUserQuestion", map[string]any{"questions": []map[string]any{{"question": "Pick a color", "options": []map[string]string{{"label": "red"}, {"label": "blue"}}}}})
		case line == "block":
			hook("PermissionRequest", map[string]any{turnKey: fmt.Sprintf("t%d", n), "tool_name": "Bash"})
		default:
			turn = fmt.Sprintf("t%d", n)
			hook("UserPromptSubmit", map[string]any{turnKey: turn, "prompt": line})
			ends = time.After(time.Duration(turnMS) * time.Millisecond)
		}
	}
	input := readInput(os.Getenv("FAKE_KEYS") == "1")
loop:
	for {
		select {
		case in, ok := <-input:
			switch {
			case !ok:
				break loop
			case in.escape && ends != nil:
				// Claude Code reports an interrupt to no hook; Codex does.
				ends = nil
				record("interrupted")
				if agent == "codex" {
					hook("Interrupt", map[string]any{turnKey: turn})
				}
			case in.escape:
				record("escape")
			case ends != nil:
				waiting = append(waiting, in.line)
			default:
				handle(in.line)
			}
		case <-ends:
			ends = nil
			hook("Stop", map[string]any{turnKey: turn, "last_assistant_message": "done"})
		case d := <-decided:
			// Claude Code ignores what a hook prints once its dialog closed.
			if d.prompt != dialog || len(d.out) == 0 {
				continue
			}
			var out struct {
				HookSpecificOutput struct {
					Decision struct {
						Behavior     string `json:"behavior"`
						Message      string `json:"message"`
						UpdatedInput struct {
							Answers map[string]string `json:"answers"`
						} `json:"updatedInput"`
					} `json:"decision"`
				} `json:"hookSpecificOutput"`
			}
			if json.Unmarshal(d.out, &out) != nil {
				continue
			}
			dec := out.HookSpecificOutput.Decision
			got := "hook:" + dec.Behavior
			if dec.Message != "" {
				got += ":" + dec.Message
			}
			for q, a := range dec.UpdatedInput.Answers {
				got += " " + q + "=" + a
			}
			record(got)
			settle(dec.Behavior == "allow")
		}
		for ends == nil && len(waiting) > 0 {
			line := waiting[0]
			waiting = waiting[1:]
			handle(line)
		}
	}
	hook("SessionEnd", map[string]any{"reason": "other"})
}

// prompt is a permission dialog of a tool call.
type prompt struct{ tool, call string }

// decision is what an answer hook printed for a dialog.
type decision struct {
	prompt *prompt
	out    []byte
}

// keyInput is a line typed into the agent, or the Escape key.
type keyInput struct {
	line   string
	escape bool
}

// readInput delivers the terminal's input. By default the terminal is left
// in canonical mode and delivers lines. With keys set it reads keys as they
// come, like a TUI: Escape alone, Enter ending a line, Ctrl+D ending input.
func readInput(keys bool) <-chan keyInput {
	ch := make(chan keyInput)
	go func() {
		defer close(ch)
		if !keys {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				ch <- keyInput{line: sc.Text()}
			}
			return
		}
		stty := exec.Command("stty", "-icanon", "-echo", "min", "1", "time", "0")
		stty.Stdin = os.Stdin
		if err := stty.Run(); err != nil {
			return
		}
		r := bufio.NewReader(os.Stdin)
		var line []byte
		for {
			b, err := r.ReadByte()
			switch {
			case err != nil || b == 0x04:
				return
			case b == 0x1b:
				ch <- keyInput{escape: true}
			case b == '\n' || b == '\r':
				ch <- keyInput{line: string(line)}
				line = line[:0]
			default:
				line = append(line, b)
			}
		}
	}()
	return ch
}

// daemon imitates Codex's app-server daemon, which runs every thread's hooks
// and serves its control socket.
func daemon() {
	d := &fakeDaemon{status: map[string]string{}, turns: map[string]string{}, stops: map[string]*time.Timer{}}
	d.turnMS, _ = strconv.Atoi(os.Getenv("FAKE_TURN_MS"))
	d.cwd, _ = os.Getwd()
	go d.serve()
	if session := os.Getenv("FAKE_SESSION"); session != "" && os.Getenv("FAKE_HOOKS") == "1" {
		d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": session, "source": "startup"})
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var payload map[string]any
		if json.Unmarshal(sc.Bytes(), &payload) == nil {
			d.hook(payload)
		}
		fmt.Println("ran")
	}
	// A daemon outlives its clients; stay until killed.
	select {}
}

type fakeDaemon struct {
	mu     sync.Mutex // one hook at a time, as the state it keeps
	cwd    string
	turnMS int
	n      int
	status map[string]string      // thread id → idle or active
	turns  map[string]string      // thread id → its active turn
	stops  map[string]*time.Timer // thread id → the end of its running turn
}

// hook runs `tmag hook codex` for payload as the daemon's child, and keeps
// the thread's status as the daemon would.
func (d *fakeDaemon) hook(payload map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.run(payload)
}

func (d *fakeDaemon) run(payload map[string]any) {
	if _, ok := payload["cwd"]; !ok {
		payload["cwd"] = d.cwd
	}
	b, _ := json.Marshal(payload)
	cmd := exec.Command(os.Getenv("TMAG_BIN"), "hook", "codex")
	cmd.Stdin = strings.NewReader(string(b))
	_ = cmd.Run()
	thread, _ := payload["session_id"].(string)
	turn, _ := payload["turn_id"].(string)
	switch payload["hook_event_name"] {
	case "SessionStart", "Stop", "Interrupt":
		d.status[thread] = "idle"
		delete(d.turns, thread)
	case "UserPromptSubmit":
		d.status[thread], d.turns[thread] = "active", turn
	}
}

// serve answers the control socket at $CODEX_HOME/app-server-control.
func (d *fakeDaemon) serve() {
	// Like Codex, listen on a short path and link it from CODEX_HOME.
	dir, err := os.MkdirTemp(os.Getenv("TMUX_TMPDIR"), "fake-daemon")
	if err != nil {
		return
	}
	real := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", real)
	if err != nil {
		return
	}
	sock := codexsvc.Socket()
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	_ = os.Remove(sock)
	if os.Symlink(real, sock) != nil {
		return
	}
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
				d.answer(c, m)
			}
		}()
	}
}

func (d *fakeDaemon) answer(c *svctest.Conn, m svctest.Message) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(m.Params, &p)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch m.Method {
	case "initialize":
		_ = c.Reply(m.ID, map[string]string{"userAgent": "fake"})
	case "thread/read":
		st := d.status[p.ThreadID]
		if st == "" {
			st = "notLoaded"
		}
		_ = c.Reply(m.ID, map[string]any{"thread": map[string]any{"id": p.ThreadID, "status": map[string]string{"type": st}}})
	case "turn/start":
		if turn, busy := d.turns[p.ThreadID]; busy {
			_ = c.Reply(m.ID, map[string]any{"turn": map[string]string{"id": turn}}) // steers
			return
		}
		d.n++
		thread, turn := p.ThreadID, fmt.Sprintf("turn-%d", d.n)
		d.run(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": thread, "turn_id": turn})
		_ = c.Reply(m.ID, map[string]any{"turn": map[string]string{"id": turn}})
		stop := func() {
			if d.turns[thread] == turn {
				d.run(map[string]any{"hook_event_name": "Stop", "session_id": thread, "turn_id": turn})
			}
		}
		if d.turnMS <= 0 {
			stop()
			return
		}
		d.stops[thread] = time.AfterFunc(time.Duration(d.turnMS)*time.Millisecond, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			stop()
		})
	case "turn/interrupt":
		if d.turns[p.ThreadID] != p.TurnID {
			_ = c.Fail(m.ID, fmt.Sprintf("expected active turn id %s but found %s", p.TurnID, d.turns[p.ThreadID]))
			return
		}
		if t := d.stops[p.ThreadID]; t != nil {
			t.Stop()
		}
		d.run(map[string]any{"hook_event_name": "Interrupt", "session_id": p.ThreadID, "turn_id": p.TurnID})
		_ = c.Reply(m.ID, map[string]any{})
	default:
		_ = c.Fail(m.ID, "unknown method "+m.Method)
	}
}

// hostCLI imitates the agent's own command line for tmag setup and doctor.
func hostCLI(args []string) {
	if args[0] == "--version" {
		fmt.Println("fake 1.0")
		return
	}
	if len(args) >= 2 && args[1] == "list" {
		list := os.Getenv("FAKE_PLUGIN_LIST")
		if list == "" {
			list = "[]"
		}
		fmt.Println(list)
		return
	}
	f, err := os.OpenFile(os.Getenv("FAKE_PLUGIN_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(1)
	}
	defer f.Close()
	fmt.Fprintln(f, filepath.Base(os.Args[0])+" "+strings.Join(args, " "))
}
