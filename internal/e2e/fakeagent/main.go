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
//	TMAG_BIN      tmag binary
//
// Lines starting with "/" behave like slash commands: no turn, no hooks. The
// line "block" raises a permission prompt that the next line resolves.
//
// Run as "codex app-server ..." it imitates Codex's shared daemon instead: it
// runs `tmag hook codex` for every JSON payload line on stdin, as its own
// child, outside any pane's agent. With FAKE_HOOKS=1 and FAKE_SESSION set it
// first reports that session's start.
//
// As a host CLI it answers --version, prints FAKE_PLUGIN_LIST (default "[]")
// for "plugin list --json", and appends any other plugin command to
// FAKE_PLUGIN_LOG.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	hook := func(event string, extra map[string]any) {
		if os.Getenv("FAKE_HOOKS") != "1" {
			return
		}
		payload := map[string]any{"hook_event_name": event, "session_id": session, "cwd": cwd, "transcript_path": filepath.Join(cwd, session+".jsonl")}
		for k, v := range extra {
			payload[k] = v
		}
		b, _ := json.Marshal(payload)
		cmd := exec.Command(os.Getenv("TMAG_BIN"), "hook", agent)
		cmd.Stdin = strings.NewReader(string(b))
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
	)
	handle := func(line string) {
		n++
		record(line)
		switch {
		case strings.HasPrefix(line, "/"):
			// slash commands start no turn
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
		}
		for ends == nil && len(waiting) > 0 {
			line := waiting[0]
			waiting = waiting[1:]
			handle(line)
		}
	}
	hook("SessionEnd", map[string]any{"reason": "other"})
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

// daemon imitates Codex's app-server daemon, which runs every thread's hooks.
func daemon() {
	run := func(payload []byte) {
		cmd := exec.Command(os.Getenv("TMAG_BIN"), "hook", "codex")
		cmd.Stdin = strings.NewReader(string(payload))
		_ = cmd.Run()
	}
	if session := os.Getenv("FAKE_SESSION"); session != "" && os.Getenv("FAKE_HOOKS") == "1" {
		cwd, _ := os.Getwd()
		b, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": session, "cwd": cwd, "source": "startup"})
		run(b)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		run(sc.Bytes())
		fmt.Println("ran")
	}
	// A daemon outlives its clients; stay until killed.
	select {}
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
