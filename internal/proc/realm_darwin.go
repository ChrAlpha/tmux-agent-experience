package proc

import (
	"context"
	"syscall"
)

// realmSource is the boot session: macOS has one process table per boot.
func realmSource() string {
	source, _ := realmSourceContext(context.Background())
	return source
}

func realmSourceContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	return id, ctx.Err()
}
