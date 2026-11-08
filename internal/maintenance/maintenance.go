// Package maintenance shares a remaining operation budget among independently
// bounded artifact classes. It never extends that parent's deadline.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

// Run advances the first class before starting work, so recurring short parent
// budgets cannot always prefer session temps over sockets or retired locks.
// Each class has a 50ms ceiling; a pass has a 150ms cooperative ceiling.
func Run(ctx context.Context, root string) error {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	first := uint64(0)
	err := lockfile.Within(ctx, root, "coord/maintenance-lock", false, false, func() error {
		b, err := statefile.ReadSmallWithin(ctx, root, "coord/maintenance", 8)
		if err == nil {
			first, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 2)
			if err != nil || first > 2 {
				return errors.New("invalid maintenance cursor")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return statefile.WriteWithin(ctx, root, "coord/maintenance", "coord/maintenance.tmp", []byte(fmt.Sprintf("%d\n", (first+1)%3)))
	})
	if errors.Is(err, lockfile.ErrBusy) {
		return nil
	}
	if err != nil {
		if parentCtx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	var result error
	for i := range 3 {
		if err := ctx.Err(); err != nil {
			if parentCtx.Err() == nil {
				return result
			}
			return errors.Join(result, parentCtx.Err())
		}
		switch (int(first) + i) % 3 {
		case 0:
			_, err = (session.Store{Dir: root}).CleanupTemps(ctx)
		case 1:
			_, err = coord.CleanupRetired(ctx, root)
		case 2:
			_, err = answer.Cleanup(ctx, root)
		}
		if ctx.Err() != nil && parentCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return result
		}
		result = errors.Join(result, err)
	}
	return result
}
