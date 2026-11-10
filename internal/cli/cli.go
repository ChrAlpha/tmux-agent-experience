// Package cli implements the tmag command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitRefused = 3
	ExitTimeout = 4
)

const usage = `tmag — agent awareness for tmux

Usage:
  tmag list     [--json]                       agents in all tmux panes
  tmag inspect  <target> [--json] [--events N] one agent and its recent events
  tmag watch    [--since SEQ]                  JSON lines: snapshot, then events
  tmag read     <target> [--lines N | --last] [--json]
                                               recent pane output, or the agent's last reply
  tmag send     <target> [--wait | --reply] [--timeout D] [--json] [text | -]
                                               deliver a prompt now, or wait for its turn
  tmag reply    <target> [--timeout D] [--json] [text | -]
                                               answer an agent that asked for a reply
  tmag interrupt <target> [--json]             stop the agent's running turn
  tmag request  <target> <request> [--json]    what an open request asks
  tmag answer   <target> <request> allow|deny [--message M] [--answers JSON] [--json]
                                               decide an open request
  tmag wait     <target> [--for STATES] [--timeout D] [--json]
                                               wait for a state (default idle,blocked)
  tmag status   [--tmux] [--server S]          one-line summary for the status line
  tmag prune    [--server S]                   reconcile panes with the sessions
  tmag jump     [--client C] [--server S] [--from %N] [--json]
                                               go to the agent waiting longest
  tmag notify   [--json]                       send a test alert through @tmag-notify
  tmag serve    [--addr ADDR] [--host HOST]... [--origin ORIGIN]...
                                               local HTTP API, until stopped
  tmag auth     issue|pair LABEL [--ttl D] [--json]
                list [--json] | revoke ID|--all [--json]
                                               manage owner client credentials
  tmag setup    [--yes] [--dry-run] [--agent claude|codex|pi|omp]
                                               install or update the agent plugins
  tmag doctor   [--json]                       check the installation
  tmag uninstall [--purge]                     print removal steps; --purge clears tmag state
  tmag hook     <claude|codex|pi|omp>          agent hook entry point (reads stdin)
  tmag answer-hook <claude>                    Claude's PermissionRequest hook: waits for answer
  tmag version

Targets: an agent id or unique prefix (>= 4 chars), a pane id %N on the current
server, or SERVER:%N where SERVER is a tmux socket name or path.

Exit codes: 0 ok, 1 error, 2 usage, 3 refused by the safety gate, 4 timeout.
`

// app carries process-wide dependencies so commands stay testable.
type app struct {
	ctx       context.Context
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	log       *eventlog.Log
	json      bool
	setup     context.Context
	setupDone func()
}

// Main runs tmag with the given arguments (without the program name).
func Main(args []string) int {
	ctx, stop := signalContext()
	defer stop()
	a := &app{ctx: ctx, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	if len(args) > 0 && (args[0] == "hook" || args[0] == "answer-hook") {
		// Process containment belongs only to this CLI entry point. Cooperative
		// work stops early enough to close pipes, reap helpers and release locks.
		cutoff := time.AfterFunc(hook.Deadline, func() { os.Exit(ExitOK) })
		defer cutoff.Stop()
		setup, cancel := context.WithTimeout(ctx, hook.Deadline-250*time.Millisecond)
		defer cancel()
		a.setup = setup
		a.setupDone = func() { cutoff.Reset(answerWindow); cancel() }
		// A signal also has a final fail-open bound if a filesystem syscall
		// cannot cooperate. No reusable function or HTTP handler exits a process.
		returned := make(chan struct{})
		defer close(returned)
		endSignal := context.AfterFunc(ctx, func() {
			timer := time.NewTimer(250 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-returned:
			case <-timer.C:
				os.Exit(ExitOK)
			}
		})
		defer endSignal()
		input, err := contextio.Own(setup, os.Stdin)
		if err != nil {
			return ExitOK
		}
		defer input.Close()
		a.stdin = input
	}
	// In hook invocations the budget and final cutoff already cover realm
	// discovery, including its filesystem accesses.
	discovery := ctx
	if a.setup != nil {
		discovery = a.setup
	}
	dir, err := eventlog.DefaultDirContext(discovery)
	if err != nil {
		if a.setup != nil {
			return ExitOK
		}
		fmt.Fprintf(a.stderr, "tmag: state directory: %v\n", err)
		return ExitError
	}
	a.log = eventlog.Open(dir)
	return a.run(args)
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "hook":
		return a.cmdHook(rest)
	case "list", "ls":
		return a.cmdList(rest)
	case "inspect":
		return a.cmdInspect(rest)
	case "watch":
		return a.cmdWatch(rest)
	case "read":
		return a.cmdRead(rest)
	case "send":
		return a.cmdSend(rest)
	case "reply":
		return a.cmdReply(rest)
	case "interrupt":
		return a.cmdInterrupt(rest)
	case "answer":
		return a.cmdAnswer(rest)
	case "request":
		return a.cmdRequest(rest)
	case "answer-hook":
		return a.cmdAnswerHook(rest)
	case "wait":
		return a.cmdWait(rest)
	case "status":
		return a.cmdStatus(rest)
	case "prune":
		return a.cmdPrune(rest)
	case "jump":
		return a.cmdJump(rest)
	case "notify":
		return a.cmdNotify(rest)
	case "auth":
		return a.cmdAuth(rest)
	case "serve":
		return a.cmdServe(rest)
	case "setup":
		return a.cmdSetup(rest)
	case "doctor":
		return a.cmdDoctor(rest)
	case "uninstall":
		return a.cmdUninstall(rest)
	case "version", "--version", "-V":
		fmt.Fprintf(a.stdout, "tmag %s (%s)\n", version.Version, version.Schema)
		return ExitOK
	case "help", "--help", "-h":
		fmt.Fprint(a.stdout, usage)
		return ExitOK
	}
	fmt.Fprintf(a.stderr, "tmag: unknown command %q\n\n%s", cmd, usage)
	return ExitUsage
}

// parse parses flags interleaved with positional arguments. Everything after a
// literal "--" is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var tail []string
	for i, arg := range args {
		if arg == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (a *app) usageError(cmd string, err error) int {
	if a.json {
		a.emit(map[string]any{"error": map[string]string{"code": "usage", "message": err.Error()}})
	}
	fmt.Fprintf(a.stderr, "tmag %s: %v\n", cmd, err)
	return ExitUsage
}

// fail reports err and returns the matching exit code.
func (a *app) fail(cmd string, err error) int { return a.failOn(cmd, "", err) }

// failOn reports err from cmd on target, then what to do next when there is
// something to say (next), and returns the matching exit code.
func (a *app) failOn(cmd, target string, err error) int {
	code, exit := "error", ExitError
	var record *session.RecordError
	var refused *query.RefusedError
	switch {
	case errors.As(err, &record):
		code = "records_unavailable"
	case errors.As(err, &refused):
		code, exit = "refused:"+refused.Reason, ExitRefused
	case errors.Is(err, errTimeout), errors.Is(err, context.DeadlineExceeded):
		code, exit = "timeout", ExitTimeout
	case errors.Is(err, query.ErrMarker):
		code, exit = "usage", ExitUsage
	case errors.Is(err, query.ErrNoReply):
		code = "no_reply"
	case errors.Is(err, query.ErrNotFound):
		code = "not_found"
	case errors.Is(err, query.ErrAmbiguous):
		code = "ambiguous"
	case errors.Is(err, query.ErrEnded):
		code = "ended"
	}
	if a.json {
		a.emit(map[string]any{"error": map[string]string{"code": code, "message": err.Error()}})
	}
	fmt.Fprintf(a.stderr, "tmag %s: %v\n", cmd, err)
	if n := next(cmd, target, err); n != "" {
		fmt.Fprintf(a.stderr, "%s%s\n", nextPrefix, n)
	}
	return exit
}

var errTimeout = errors.New("timed out")

// emit writes one JSON document carrying the contract schema.
func (a *app) emit(fields map[string]any) {
	doc := map[string]any{"schema": version.Schema}
	for k, v := range fields {
		doc[k] = v
	}
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(doc)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
