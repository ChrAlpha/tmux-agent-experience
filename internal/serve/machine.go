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

// command runs tmag on the machine. Through ssh the remote shell splits the
// arguments again, so each is quoted for it.
func (m Machine) command(ctx context.Context, args ...string) *exec.Cmd {
	if filepath.Base(m.Argv[0]) == "ssh" {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		args = quoted
	}
	argv := append(slices.Clone(m.Argv), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Let tmag, or ssh, end on its own terms first.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
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
