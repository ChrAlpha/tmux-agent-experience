package eventlog

import (
	"context"
	"os"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
)

// publish replaces one checkpoint under the log lock, including its fixed
// temporary pathname. A canceled publication leaves at most that one temporary;
// the next publisher reuses it. No syscall can wait for a FIFO reader/writer.
func (l *Log) publish(ctx context.Context, name string, data []byte) error {
	tmp := l.path(name + ".tmp")
	f, err := contextio.OpenRegular(ctx, tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = contextio.WriteAll(ctx, f, data)
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(name))
}
