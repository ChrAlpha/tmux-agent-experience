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
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

var errPassBudget = errors.New("maintenance budget exhausted")

type worker func(context.Context) (Work, error)

func workers(root string) [3]worker {
	return [3]worker{
		func(ctx context.Context) (Work, error) {
			r, err := (session.Store{Dir: root}).CleanupTemps(ctx)
			return Work{Examined: uint64(r.Examined), Removed: uint64(r.Removed), Deferrals: uint64(r.Deferred)}, err
		},
		func(ctx context.Context) (Work, error) {
			r, err := coord.CleanupRetired(ctx, root)
			return Work{Examined: uint64(r.Examined), Removed: uint64(r.Removed), Deferrals: uint64(r.Deferred)}, err
		},
		func(ctx context.Context) (Work, error) {
			r, err := answer.Cleanup(ctx, root)
			return Work{Examined: uint64(r.Examined), Removed: uint64(r.Removed), Deferrals: uint64(r.Deferred)}, err
		},
	}
}

// Run preserves the legacy cursor and phase rotation. Status publication is
// best effort, fixed-size and excluded separately, within the same 150ms pass.
func Run(ctx context.Context, root string) error {
	return run(ctx, root, policy.Resolve, workers(root))
}
func run(parent context.Context, root string, resolve func(context.Context, policy.Spec) (int, error), classes [3]worker) (result error) {
	ctx, cancel := context.WithTimeoutCause(parent, 150*time.Millisecond, errPassBudget)
	defer cancel()
	enabled, policyErr := resolve(ctx, policy.Diagnostics)
	recording := policyErr == nil && enabled != 0
	var work Work
	var ticket uint64
	defer func() {
		if errors.Is(context.Cause(ctx), errPassBudget) {
			result = contextio.WithoutContextError(result, ctx.Err())
		}
		if parent.Err() != nil {
			result = errors.Join(result, parent.Err())
		}
		if !recording {
			return
		}
		outcome, failure := "completed", ""
		switch {
		case result != nil:
			outcome = "failed"
			failure = "io"
			switch {
			case parent.Err() != nil:
				outcome = "interrupted"
				failure = "deferred"
			case errors.Is(result, ownedfs.ErrScope):
				failure = "unsafe_object"
			case errors.Is(result, context.DeadlineExceeded):
				failure = "timeout"
			}
		case work.Deferrals > 0 || work.Classes < 3:
			outcome = "deferred"
		}
		_ = completed(ctx, root, ticket, work, outcome, failure)
		if result != nil {
			_ = diagnostic.Record(ctx, root, diagnostic.Fault{Component: "maintenance", Operation: "run", Category: failure})
		}
	}()
	first := uint64(0)
	err := lockfile.Within(ctx, root, "coord/maintenance-lock", false, false, func() error {
		if recording {
			ticket, _ = admitted(ctx, root)
		}
		b, err := statefile.ReadSmallWithin(ctx, root, "coord/maintenance", 8)
		if err == nil {
			first, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 2)
			if err != nil || first > 2 {
				return errors.New("invalid maintenance cursor")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := statefile.WriteWithin(ctx, root, "coord/maintenance", "coord/maintenance.tmp", []byte(fmt.Sprintf("%d\n", (first+1)%3))); err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, lockfile.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range 3 {
		if ctx.Err() != nil {
			work.Deferrals += uint64(3 - i)
			return errors.Join(result, ctx.Err())
		}
		r, err := classes[(int(first)+i)%3](ctx)
		work.Classes++
		work.Examined += r.Examined
		work.Removed += r.Removed
		work.Deferrals += r.Deferrals
		if ctx.Err() != nil {
			work.Deferrals++
		}
		result = errors.Join(result, err)
	}
	return result
}
