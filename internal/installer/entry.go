package installer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// RunChild dispatches only the private installer entry point, before CLI setup.
// The enclosing Bash group supplies the whole-attempt deadline. This local
// ceiling is an additional bound, never an extension of the enclosing attempt.
func RunChild(args []string) (bool, error) {
	if len(args) == 0 || (args[0] != "__install-release" && args[0] != "__install-capabilities") {
		return false, nil
	}
	cutoff := time.AfterFunc(10*time.Second, func() { os.Exit(1) })
	defer cutoff.Stop()
	if args[0] == "__install-capabilities" {
		if len(args) != 1 {
			return true, fmt.Errorf("invalid installer capability invocation")
		}
		_, err := fmt.Fprintln(os.Stdout, "tmag/install/v1")
		return true, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()
	if len(args) != 5 {
		return true, fmt.Errorf("invalid private installer invocation")
	}
	parent := os.Getppid()
	if parent <= 1 {
		return true, ErrOwnership
	}
	controller, err := strconv.Atoi(args[3])
	if err != nil || controller <= 1 {
		return true, ErrOwnership
	}
	source, err := os.Executable()
	if err != nil {
		return true, err
	}
	live := func(ctx context.Context) error {
		if os.Getppid() != parent {
			return ErrOwnership
		}
		return controllerAlive(ctx, parent, controller)
	}
	return true, Publish(ctx, Config{Root: args[1], Want: args[2], Compiled: version.Version, Source: source, Digest: args[4], Live: live})
}

type processOutput struct{ bytes.Buffer }

func (b *processOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, ErrOwnership
	}
	return b.Buffer.Write(p)
}

// ps emits only numeric ancestry metadata. Unlike session-discovery helpers it
// stays in the installer's existing group, so the outer cutoff contains it too.
func controllerAlive(ctx context.Context, parent, controller int) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "ppid=")
	var output processOutput
	cmd.Stdout = &output
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return ErrOwnership
	}
	parents := make(map[int]int)
	for _, line := range strings.Split(output.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		id, e1 := strconv.Atoi(fields[0])
		ppid, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil {
			return ErrOwnership
		}
		parents[id] = ppid
	}
	if os.Getppid() != parent {
		return ErrOwnership
	}
	for id, depth := parent, 0; id > 1 && depth < 256; depth++ {
		if id == controller {
			return nil
		}
		next := parents[id]
		if next == id {
			return ErrOwnership
		}
		id = next
	}
	return ErrOwnership
}
