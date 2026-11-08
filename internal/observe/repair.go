package observe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/dirscan"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

const repairLimit = 128
const repairBudget = 300 * time.Millisecond

var errRepairBudget = errors.New("repair budget exhausted")

// repairIO is the bounded metadata I/O seam. Resource mutation and proofs still
// run through Env; tests can stop a specific scan/publication at its deadline.
type repairIO struct {
	readProgress func(context.Context) (progress, error)
	saveProgress func(context.Context, progress) error
	scanCatalog  func(context.Context) (session.Catalog, error)
}

func repairOperations(env attach.Env) repairIO {
	return repairIO{
		readProgress: func(ctx context.Context) (progress, error) { return readProgress(ctx, env.Log.Dir) },
		saveProgress: func(ctx context.Context, p progress) error { return save(ctx, env.Log.Dir, "progress", p) },
		scanCatalog: func(ctx context.Context) (session.Catalog, error) {
			return env.Store.ScanBoundedContext(ctx, MaxImageBytes)
		},
	}
}

type progress struct {
	Version  int
	First    int
	Sessions [2]dirscan.Cursor
	Pane     int
}

func readProgress(ctx context.Context, root string) (progress, error) {
	var p progress
	b, err := statefile.ReadSmallWithin(ctx, root, "observe/progress.json", 32<<10)
	if errors.Is(err, os.ErrNotExist) {
		return progress{Version: 1}, nil
	}
	if err != nil {
		return p, err
	}
	if json.Unmarshal(b, &p) != nil || p.Version != 1 || p.First < 0 || p.First > 2 || p.Pane < 0 || !p.Sessions[0].Valid() || !p.Sessions[1].Valid() {
		return p, ErrUnavailable
	}
	return p, nil
}
func merge(to *attach.ReconcileReport, from attach.ReconcileReport) {
	to.Examined += from.Examined
	to.Repaired += from.Repaired
	to.Deferred += from.Deferred
	to.FaultCount += from.FaultCount
	to.Err = errors.Join(to.Err, from.Err)
	to.Complete = to.Complete && from.Complete
	for _, f := range from.Faults {
		if len(to.Faults) < 16 {
			to.Faults = append(to.Faults, f)
		}
	}
}
func attempt(ctx context.Context, report *attach.ReconcileReport, operation, agent, id string, err error) {
	report.Examined++
	if err == nil {
		report.Repaired++
		return
	}
	report.Complete = false
	expired := ctx.Err()
	if expired != nil && contextio.WithoutContextError(err, expired) == nil {
		report.Deferred++
		return
	}
	report.FaultCount++
	category := "io"
	var issue *session.RecordIssue
	if errors.As(err, &issue) {
		category = issue.Category
	}
	if len(report.Faults) < 16 {
		report.Faults = append(report.Faults, attach.Fault{Operation: operation, Category: category, Agent: agent, ID: id})
	}
}

// repair rotates phases and persists each next position BEFORE attempting a
// resource. A hot/failed lock cannot monopolize the prefix across invocations.
// Files and pane markers remain the durable repair queue, independent of cache.
func repair(parent context.Context, env attach.Env, w *attach.World) (attach.ReconcileReport, error) {
	return repairUsing(parent, env, w, repairOperations(env))
}
func repairUsing(parent context.Context, env attach.Env, w *attach.World, ops repairIO) (report attach.ReconcileReport, resultErr error) {
	ctx, cancel := context.WithTimeoutCause(parent, repairBudget, errRepairBudget)
	defer cancel()
	report.Complete = true
	var earlierTimeouts error
	// Every early return and phase uses the same budget boundary. Strip only
	// this worker's context leaves; preserve joined real failures and add an
	// actual parent cancellation/deadline independently.
	defer func() {
		if ctx.Err() != nil {
			report.Complete = false
			if report.Deferred == 0 {
				report.Deferred++
			}
		}
		if errors.Is(context.Cause(ctx), errRepairBudget) {
			resultErr = errors.Join(contextio.WithoutContextError(resultErr, ctx.Err()), earlierTimeouts)
		}
		if parent.Err() != nil {
			resultErr = errors.Join(resultErr, parent.Err())
		}
		report.Err = resultErr
	}()
	noteAttempt := func(operation, agent, id string, err error) {
		// A helper timeout observed while this budget is still healthy is a real
		// failure, even if the repair budget expires during a later operation.
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			earlierTimeouts = errors.Join(earlierTimeouts, err)
		}
		attempt(ctx, &report, operation, agent, id, err)
	}
	if w.Proc == nil || !w.Proc.Complete() {
		report.Complete = false
		return report, ErrUnavailable
	}
	root := env.Log.Dir
	p, err := ops.readProgress(ctx)
	if err != nil {
		noteAttempt("scan", "", "", err)
		return report, err
	}
	first := p.First
	p.First = (p.First + 1) % 3
	if err := ops.saveProgress(ctx, p); err != nil {
		noteAttempt("scan", "", "", err)
		return report, err
	}
	panes := append([]tmux.Pane(nil), w.Panes...)
	if env.PaneServer != "" {
		panes = slices.DeleteFunc(panes, func(p tmux.Pane) bool { return p.Server != env.PaneServer })
	}
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].Server != panes[j].Server {
			return panes[i].Server < panes[j].Server
		}
		return panes[i].ID < panes[j].ID
	})
	examined := 0
	var faults error
	for phaseIndex := range 3 {
		phase := (first + phaseIndex) % 3
		if ctx.Err() != nil || examined >= repairLimit {
			report.Complete = false
			report.Deferred++
			break
		}
		if phase == 2 {
			c, scanErr := ops.scanCatalog(ctx)
			if scanErr != nil {
				faults = errors.Join(faults, scanErr)
				noteAttempt("scan", "", "", scanErr)
				continue
			}
			if len(panes) == 0 {
				p.Pane = 0
				continue
			}
			if p.Pane >= len(panes) {
				p.Pane = 0
			}
			start := p.Pane
			for {
				if ctx.Err() != nil || examined >= repairLimit {
					report.Complete = false
					report.Deferred++
					break
				}
				current := p.Pane
				p.Pane = (p.Pane + 1) % len(panes)
				if err := ops.saveProgress(ctx, p); err != nil {
					noteAttempt("scan", "", "", err)
					return report, errors.Join(faults, err)
				}
				pr := env.RepairPanes(ctx, w, c, panes[current:current+1])
				merge(&report, pr)
				// Pane errors already exclude pure expiration at each operation boundary.
				if errors.Is(pr.Err, context.DeadlineExceeded) {
					earlierTimeouts = errors.Join(earlierTimeouts, pr.Err)
				}
				examined++
				faults = errors.Join(faults, pr.Err)
				if p.Pane == start {
					break
				}
			}
			continue
		}
		agent := []string{proc.Claude, proc.Codex}[phase]
		d, openErr := ownedfs.Open(ctx, root, "sessions/"+agent, false)
		if errors.Is(openErr, os.ErrNotExist) {
			p.Sessions[phase] = dirscan.Cursor{}
			continue
		}
		if openErr != nil {
			faults = errors.Join(faults, openErr)
			noteAttempt("scan", agent, "", openErr)
			continue
		}
		for ctx.Err() == nil && examined < repairLimit {
			names, next, _, scanErr := dirscan.Next(ctx, d, p.Sessions[phase], 1)
			if scanErr != nil {
				faults = errors.Join(faults, scanErr)
				noteAttempt("scan", agent, "", scanErr)
				break
			}
			p.Sessions[phase] = next
			if err := ops.saveProgress(ctx, p); err != nil {
				noteAttempt("scan", agent, "", err)
				_ = d.Close()
				return report, errors.Join(faults, err)
			}
			if len(names) == 0 && next.Offset == 0 && len(next.Pending) == 0 {
				break
			}
			if len(names) == 0 {
				continue
			}
			examined++
			id, ok := strings.CutSuffix(names[0], ".json")
			if !ok || !session.ValidID(id) {
				continue
			}
			r, found, readErr := env.Store.GetContext(ctx, agent, id)
			if readErr != nil {
				noteAttempt("scan", agent, id, readErr)
				faults = errors.Join(faults, readErr)
				continue
			}
			if found {
				err := env.RepairRecord(ctx, w, r)
				noteAttempt("session", agent, id, err)
				if err != nil {
					faults = errors.Join(faults, err)
				}
			}
		}
		_ = d.Close()
	}
	if ctx.Err() != nil || examined >= repairLimit {
		report.Complete = false
	}
	// Cooperative repair-budget deferral is reported separately from a failure.
	return report, faults
}
