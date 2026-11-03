// Package cli implements the tmag command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
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
  tmag read     <target> [--lines N] [--json]  recent pane output
  tmag send     <target> [--wait] [--timeout D] [--json] [text | -]
                                               submit a prompt to an idle agent
  tmag wait     <target> [--for STATES] [--after SEQ] [--timeout D] [--json]
                                               wait for a state (default idle,blocked)
  tmag status   [--tmux] [--server S]          one-line summary for the status line
  tmag jump     [--client C] [--server S] [--from %N] [--json]
                                               go to the agent waiting longest
  tmag setup    [--yes] [--dry-run] [--agent claude|codex]
                                               install or update the agent plugins
  tmag doctor   [--json]                       check the installation
  tmag uninstall [--purge]                     print removal steps; --purge clears tmag state
  tmag hook     <claude|codex>                 agent hook entry point (reads stdin)
  tmag version

Targets: an agent id or unique prefix (>= 4 chars), a pane id %N on the current
server, or SERVER:%N where SERVER is a tmux socket name or path.

Exit codes: 0 ok, 1 error, 2 usage, 3 refused by the safety gate, 4 timeout.
`

// app carries process-wide dependencies so commands stay testable.
type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	log    *eventlog.Log
	json   bool
}

// Main runs tmag with the given arguments (without the program name).
func Main(args []string) int {
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, log: eventlog.Open(eventlog.DefaultDir())}
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
	case "wait":
		return a.cmdWait(rest)
	case "status":
		return a.cmdStatus(rest)
	case "jump":
		return a.cmdJump(rest)
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
func (a *app) fail(cmd string, err error) int {
	code, exit := "error", ExitError
	var refused *query.RefusedError
	switch {
	case errors.As(err, &refused):
		code, exit = "refused:"+refused.Reason, ExitRefused
	case errors.Is(err, errTimeout):
		code, exit = "timeout", ExitTimeout
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
