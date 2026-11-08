// Package helper runs one owned helper process group within its caller's budget.
package helper

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Drain is reserved inside a deadline for exec's pipe copier shutdown.
const Drain = 50 * time.Millisecond

// Cmd configures an exec command and preserves its shortened deadline error.
type Cmd struct {
	*exec.Cmd
	ctx     context.Context
	cleanup func()
}

// Run waits for the direct child and its bounded pipe drain.
func (c *Cmd) Run() error {
	err := c.Cmd.Run()
	if c.ctx.Err() != nil {
		err = c.ctx.Err()
	}
	c.cleanup()
	return err
}

// Output captures stdout within the same lifetime as Run.
func (c *Cmd) Output() ([]byte, error) {
	out, err := c.Cmd.Output()
	if c.ctx.Err() != nil {
		err = c.ctx.Err()
	}
	c.cleanup()
	return out, err
}

// Command creates an isolated process group. The caller must defer cleanup,
// including after successful exit: a descendant may retain an output pipe.
func Command(ctx context.Context, name string, args ...string) (*Cmd, func()) {
	child := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); ok {
		child, cancel = context.WithDeadline(ctx, deadline.Add(-Drain))
	}
	cmd := exec.CommandContext(child, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = kill
	cmd.WaitDelay = Drain
	var once sync.Once
	cleanup := func() { once.Do(func() { _ = kill(); cancel() }) }
	return &Cmd{Cmd: cmd, ctx: child, cleanup: cleanup}, cleanup
}
