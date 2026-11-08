package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

const tempSweepSlots = 8
const tempSweepBudget = 50 * time.Millisecond

// Cleanup reports actual bounded work, rather than an assumed directory size.
type Cleanup struct {
	Slots    int
	Examined int
	Removed  int
}

// CleanupTemps examines fixed names only, never traversing a growing directory.
// The saved start advances one stripe before each batch, so every stripe gets
// the first turn within 256 admitted sweeps even if a time budget repeatedly
// expires partway through a batch. Unknown legacy random temps stay intact.
// Call after mandatory work, with its remaining operation context.
func (s Store) CleanupTemps(ctx context.Context) (Cleanup, error) {
	ctx, cancel := context.WithTimeout(ctx, tempSweepBudget)
	defer cancel()
	var report Cleanup
	err := lockfile.Within(ctx, s.Dir, "coord/sweep-lock", false, false, func() error {
		cursor := "coord/sweep"
		slot := uint64(0)
		b, err := statefile.ReadSmallWithin(ctx, s.Dir, cursor, 8)
		if err == nil {
			slot, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 8)
			if err != nil {
				return errors.New("invalid temporary sweep cursor")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Advance the batch's starting point independently of how much work
		// fits this time. A fixed end-of-batch skip cannot starve the same IDs.
		nextStart := (slot + 1) % coord.Slots
		if err := statefile.WriteWithin(ctx, s.Dir, cursor, cursor+".tmp", []byte(fmt.Sprintf("%d\n", nextStart))); err != nil {
			return err
		}
		for range tempSweepSlots {
			if err := ctx.Err(); err != nil {
				return err
			}
			current := byte(slot)
			slot = (slot + 1) % coord.Slots
			report.Slots++
			err := coord.TrySlot(ctx, s.Dir, coord.Session, current, func() error {
				for _, agent := range []string{proc.Claude, proc.Codex} {
					if err := ctx.Err(); err != nil {
						return err
					}
					report.Examined++
					rel := filepath.Join("sessions", agent, fmt.Sprintf(".tmag-tmp-s-%02x", current))
					removed, err := statefile.RemoveSlot(ctx, s.Dir, rel, ".tmag-tmp-s-", coord.Slots)
					if err != nil {
						return err
					}
					if removed {
						report.Removed++
					}
				}
				return nil
			})
			if err != nil && !errors.Is(err, lockfile.ErrBusy) {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, lockfile.ErrBusy) {
		return report, nil
	}
	return report, err
}
