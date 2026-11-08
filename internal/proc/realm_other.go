//go:build !darwin

package proc

import (
	"context"
	"os"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
)

// realmSource is the boot and the PID namespace: Linux shows each namespace
// its own process table, and every container on a host shares its boot id.
func realmSource() string {
	source, _ := realmSourceContext(context.Background())
	return source
}

func realmSourceContext(ctx context.Context) (string, error) {
	boot, err := contextio.ReadFile(ctx, "/proc/sys/kernel/random/boot_id", 4096)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + " " + ns, nil
}
