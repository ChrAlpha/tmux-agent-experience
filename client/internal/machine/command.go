package machine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxDoc bounds one document tmag prints; a snapshot of many sessions is the
// largest.
const maxDoc = 16 << 20

// Command reaches tmag by running a command with the verb's arguments
// appended: `tmag`, `ssh box tmag`, `docker exec -i dev tmag`.
type Command struct {
	argv []string
	// shell is set when the arguments cross ssh, whose remote shell splits
	// them again, so each is quoted for it.
	shell bool
}

// NewCommand returns the backend that runs argv.
func NewCommand(argv []string) *Command {
	return &Command{argv: slices.Clone(argv), shell: slices.ContainsFunc(argv, func(a string) bool {
		b := filepath.Base(a)
		return b == "ssh" || b == "autossh"
	})}
}

func (c *Command) String() string { return strings.Join(c.argv, " ") }

func (c *Command) command(ctx context.Context, args []string) *exec.Cmd {
	if c.shell {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		args = quoted
	}
	cmd := exec.CommandContext(ctx, c.argv[0], append(slices.Clone(c.argv[1:]), args...)...)
	// A session of its own has no terminal to ask on, so a command that
	// would prompt (a host key, a password) fails at once and says why; and
	// what it starts, ssh and the tmag at the other end, ends with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// Call runs one verb.
func (c *Command) Call(ctx context.Context, op Op) (json.RawMessage, error) {
	args, input, err := op.args()
	if err != nil {
		return nil, err
	}
	cmd := c.command(ctx, args)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	out := &capped{max: maxDoc}
	said := &capped{max: 64 << 10, tail: true}
	cmd.Stdout, cmd.Stderr = out, said
	err = cmd.Run()
	if err == nil && json.Valid(out.Bytes()) {
		return out.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, failure(out.Bytes(), said.String())
}

// Watch runs `tmag watch` and reads its documents.
func (c *Command) Watch(ctx context.Context, since int64, each func(Doc) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"watch"}
	if since >= 0 {
		args = append(args, "--since", strconv.FormatInt(since, 10))
	}
	cmd := c.command(ctx, args)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	said := &capped{max: 64 << 10, tail: true}
	cmd.Stderr = said
	if err := cmd.Start(); err != nil {
		return &Error{Code: Unreachable, Message: err.Error()}
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxDoc)
	var last []byte // the last line, which says why when tmag ends
	var stop error
	for sc.Scan() {
		last = sc.Bytes()
		var d Doc
		if json.Unmarshal(last, &d) != nil || d.Type == "" {
			continue
		}
		if stop = each(d); stop != nil {
			break
		}
	}
	last = bytes.Clone(last)
	cancel()
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()
	switch {
	case stop != nil:
		return stop
	case sc.Err() != nil:
		return &Error{Code: Unreachable, Message: sc.Err().Error()}
	}
	return failure(last, said.String())
}

// capped keeps at most max bytes of what is written to it: the first, or
// with tail the last.
type capped struct {
	bytes.Buffer
	max  int
	tail bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if c.tail {
		c.Buffer.Write(p)
		if over := c.Len() - c.max; over > 0 {
			c.Next(over)
		}
		return n, nil
	}
	if room := c.max - c.Len(); room < len(p) {
		p = p[:max(room, 0)]
	}
	c.Buffer.Write(p)
	return n, nil
}
