package answer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

func knownRegular(st os.FileInfo) bool {
	return st.Mode().IsRegular() && ownedfs.Owner(st) && st.Sys().(*syscall.Stat_t).Nlink == 1
}

// clearSlot owns its liveness lock and the creator namespace. False means an
// unknown/live object prevents reuse. It never unlinks a live or uncertain
// socket; successful cleanup leaves the slot's fixed names absent.
func clearSlot(ctx context.Context, d *ownedfs.Dir, slot byte) (bool, error) {
	temp, meta, sock := fmt.Sprintf("temp-%02x", slot), fmt.Sprintf("meta-%02x", slot), fmt.Sprintf("sock-%02x", slot)
	if st, err := d.Stat(ctx, temp); err == nil {
		if !knownRegular(st) {
			return false, nil
		}
		if err := d.Remove(ctx, temp, st); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	e, metaErr := readEndpoint(ctx, d, slot)
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		if errors.Is(metaErr, ownedfs.ErrScope) {
			return false, nil
		}
		return false, metaErr
	}
	st, err := d.Stat(ctx, sock)
	if err == nil {
		if metaErr != nil || st.Mode()&os.ModeSocket == 0 || !ownedfs.Owner(st) || st.Sys().(*syscall.Stat_t).Nlink != 1 {
			return false, nil //nolint:nilerr // Missing identity preserves an existing socket and forbids reuse.
		}
		// A dead socket refuses at once. The probe takes at most half of what
		// remains of the setup, leaving the rest to the bind it makes room for.
		probe, cancel := ctx, context.CancelFunc(func() {})
		if deadline, ok := ctx.Deadline(); ok {
			probe, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(deadline)/2))
		}
		dead := probeAt(probe, d, sock, e.Request) == Dead
		cancel()
		if !dead {
			return false, nil
		}
		if err := d.Remove(ctx, sock, st); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if metaErr == nil {
		st, err := d.Stat(ctx, meta)
		if err != nil {
			return false, err
		}
		if !knownRegular(st) {
			return false, nil
		}
		if err := d.Remove(ctx, meta, st); err != nil {
			return false, err
		}
	}
	return true, nil
}
