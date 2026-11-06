package serve

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Machine is one machine whose agents the page shows: a name, and the
// command that runs tmag there with the arguments it is given.
type Machine struct {
	Name string   `json:"name"`
	Argv []string `json:"-"`
}

var machineName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ParseMachine reads NAME=COMMAND, the command split at spaces: a command
// that needs quoting belongs in a script of its own.
func ParseMachine(s string) (Machine, error) {
	name, command, ok := strings.Cut(s, "=")
	argv := strings.Fields(command)
	switch {
	case !ok || len(argv) == 0:
		return Machine{}, fmt.Errorf("machine %q: want NAME=COMMAND, such as box=ssh box tmag", s)
	case !machineName.MatchString(name):
		return Machine{}, fmt.Errorf("machine %q: a name is letters, digits, '.', '_' and '-'", name)
	}
	return Machine{Name: name, Argv: argv}, nil
}

// command runs tmag on the machine. Through ssh, or a command that runs it
// (autossh, sshpass, tsh), the remote shell splits the arguments again, so
// each is quoted for it.
func (m Machine) command(ctx context.Context, args ...string) *exec.Cmd {
	if slices.ContainsFunc(m.Argv, func(a string) bool { b := filepath.Base(a); return b == "ssh" || b == "autossh" }) {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		args = quoted
	}
	argv := append(slices.Clone(m.Argv), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// In a session of its own: it has no terminal to ask anything on, so a
	// machine that would prompt (an unknown host key, a password) fails at
	// once and says why, rather than waiting unseen; and what a wrapper script
	// starts, ssh and the watch at the other end with it, ends with the
	// command. Let tmag, or ssh, end on its own terms first.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// lastLine is the last non-empty line of a command's error output, which
// says why it failed.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// exitError describes how a command ended, with what it said last.
func exitError(err error, stderr []byte) string {
	var exit *exec.ExitError
	if line := lastLine(stderr); line != "" {
		return line
	}
	if errors.As(err, &exit) {
		return exit.String()
	}
	return err.Error()
}
