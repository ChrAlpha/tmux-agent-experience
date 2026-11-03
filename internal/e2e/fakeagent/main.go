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
		payload := map[string]any{"hook_event_name": event, "session_id": session, "cwd": cwd}
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
	sc := bufio.NewScanner(os.Stdin)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if out := os.Getenv("FAKE_OUT"); out != "" {
			f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				fmt.Fprintln(f, line)
				f.Close()
			}
		}
		if strings.HasPrefix(line, "/") {
			continue // slash commands start no turn
		}
		if line == "block" {
			hook("PermissionRequest", map[string]any{turnKey: fmt.Sprintf("t%d", n), "tool_name": "Bash"})
			continue
		}
		turn := fmt.Sprintf("t%d", n)
		hook("UserPromptSubmit", map[string]any{turnKey: turn, "prompt": line})
		time.Sleep(time.Duration(turnMS) * time.Millisecond)
		hook("Stop", map[string]any{turnKey: turn, "last_assistant_message": "done"})
	}
	hook("SessionEnd", map[string]any{"reason": "other"})
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
