// Package notify runs the person's @tmag-notify command for an alert, the
// change a hook rings a pane's bell for, so that it reaches them away from
// the terminal too.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
)

// Option is the tmux option holding the person's command.
const Option = "@tmag-notify"

// Timeout bounds one run of the command.
const Timeout = 20 * time.Second

// Present is how recently the person must have typed into a tmux client to
// count as being at the terminal, where the bell reaches them instead.
const Present = time.Minute

// Kinds of alert besides those of @tmag-alert (blocked, finished).
const Test = "test"

// commandVar carries the command from a hook to the process that runs it.
const commandVar = "TMAG_NOTIFY_COMMAND"

// maxOutput bounds how much of the command's output is kept for a report.
const maxOutput = 4 << 10

// Alert is one change worth telling the person about. It holds ids, states,
// names and paths: never a prompt, a reply or a tool's input.
type Alert struct {
	Kind        string // blocked, finished or test
	Agent       string
	Session     string
	State       string
	StateKind   string
	Request     string // the request the session waits on, when blocked
	RequestKind string
	Tool        string
	Cwd         string
	Target      string // the pane that shows the session, as session:window.pane
	Host        string
}

// Title is a notification title for the alert.
func (a Alert) Title() string {
	switch a.Kind {
	case "blocked":
		switch a.RequestKind {
		case "question":
			return a.Agent + " has a question"
		case "plan":
			return a.Agent + " has a plan for you"
		}
		return a.Agent + " needs your approval"
	case "finished":
		if a.StateKind == "error" {
			return a.Agent + " stopped on an error"
		}
		return a.Agent + " finished"
	}
	return "tmag test"
}

// Message is a notification body for the alert: what and where.
func (a Alert) Message() string {
	var parts []string
	if a.Kind == Test {
		parts = append(parts, "a test alert from tmag notify")
	}
	if a.Tool != "" {
		parts = append(parts, a.Tool)
	}
	if a.Cwd != "" {
		parts = append(parts, filepath.Base(a.Cwd))
	}
	for _, s := range []string{a.Target, a.Host} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// Env is the alert as the variables the command gets.
func (a Alert) Env() []string {
	return []string{
		"TMAG_ALERT=" + a.Kind,
		"TMAG_TITLE=" + a.Title(),
		"TMAG_MESSAGE=" + a.Message(),
		"TMAG_AGENT=" + a.Agent,
		"TMAG_SESSION=" + a.Session,
		"TMAG_STATE=" + a.State,
		"TMAG_KIND=" + a.StateKind,
		"TMAG_REQUEST=" + a.Request,
		"TMAG_TOOL=" + a.Tool,
		"TMAG_CWD=" + a.Cwd,
		"TMAG_TARGET=" + a.Target,
		"TMAG_HOST=" + a.Host,
	}
}

// Start runs command for the alert in a detached `bin notify --alert`, which
// gives up after Timeout, and returns at once: a hook must not wait for the
// network. The process leaves the hook's session, so an agent that ends its
// hook's process group does not end it.
func Start(bin, command string, a Alert) error {
	cmd := exec.Command(bin, "notify", "--alert")
	cmd.Env = append(append(os.Environ(), a.Env()...), commandVar+"="+command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reaped, should the hook outlive it
	return nil
}

// FromEnv splits the environment Start gave a process into the command and
// the environment to run it in.
func FromEnv(environ []string) (command string, env []string) {
	env = make([]string, 0, len(environ))
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, commandVar+"="); ok {
			command = v
			continue
		}
		env = append(env, kv)
	}
	return command, env
}

// ErrTimeout is a command that did not finish in time; it was ended.
var ErrTimeout = errors.New("@tmag-notify did not finish within " + Timeout.String())

// Run runs command with sh in env until it exits or ctx ends, and returns
// what it printed. The command runs in a process group of its own, which
// ends with it, children included.
func Run(ctx context.Context, command string, env []string) ([]byte, error) {
	cmd, cleanup := helper.Command(ctx, "/bin/sh", "-c", command)
	defer cleanup()
	cmd.Env = env
	out := &capped{}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		err = ErrTimeout
	}
	return out.Bytes(), err
}

// RunAlert is `tmag notify --alert`: the process Start began. It runs the
// command it was given and describes a failure for hook.log.
func RunAlert(environ []string) error {
	command, env := FromEnv(environ)
	if command == "" {
		return errors.New("notify --alert: no command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	out, err := Run(ctx, command, env)
	if err != nil {
		return fmt.Errorf("%s: %w%s", Option, err, quote(out))
	}
	return nil
}

func quote(out []byte) string {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return ""
	}
	if i := bytes.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return fmt.Sprintf(": %q", out)
}

// capped keeps the first maxOutput bytes written to it.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
