package observe

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

type Mode int

const (
	Opportunistic Mode = iota
	Explicit
	// Retry replaces a checkpoint proven lost, within the caller's capture budget.
	Retry
	// Bootstrap may wait for an unseeded refresh already running. It shares the
	// same 1.5s operation budget and cooldown, never performs a caller retry.
	Bootstrap
	// freshRead triggers optional repair for a separately fresh complete read;
	// skipped work does not require any shared image.
	freshRead
)

type Report struct {
	Skipped string // disabled | cooldown | contended; no image or successful repair asserted
	Cached  bool
	Repair  attach.ReconcileReport
}
type refreshState struct {
	Version    int
	Attempt    time.Time
	Generation uint64
	Interval   int
	// Failure describes the last completed attempt. InProgress is a distinct
	// crash marker, interpreted as abandoned only after acquiring exclusion.
	Failure     bool
	InProgress  bool
	FailureCode string
	SuccessAt   time.Time
	CompletedAt time.Time
}

func readState(ctx context.Context, root string) (refreshState, error) {
	b, err := statefile.ReadSmallWithin(ctx, root, "observe/state.json", 4096)
	if errors.Is(err, os.ErrNotExist) {
		return refreshState{Version: 1}, nil
	}
	if err != nil {
		return refreshState{}, err
	}
	return decodeState(b)
}
func decodeState(b []byte) (refreshState, error) {
	var s refreshState
	if json.Unmarshal(b, &s) != nil || s.Version != 1 || s.Interval < 0 || s.Interval > 3600 {
		return s, ErrUnavailable
	}
	switch s.FailureCode {
	case "", "records_unavailable", "observation_capacity", "history_unavailable", "observation_unavailable":
	default:
		return refreshState{}, ErrUnavailable
	}
	return s, nil
}
func save(ctx context.Context, root, name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return statefile.WriteSlots(ctx, root, "observe/"+name+".json", "observe/."+name+"-00", "."+name+"-", 1, b)
}

// Refresh takes exclusion BEFORE discovery. Contenders never run another world
// scan or wait in an opportunistic queue. Explicit prune and an admitted
// bootstrap joining the initial publication may wait within the same budget.
func Refresh(parent context.Context, env attach.Env, mode Mode) (Image, Report, error) {
	return refresh(parent, env, mode, attach.Load, policy.Resolve)
}

// Trigger joins ordinary rate-limited housekeeping without requiring a cache
// image. Its caller must independently load a fresh complete view and proof.
func Trigger(parent context.Context, env attach.Env) (Report, error) {
	_, report, err := refresh(parent, env, freshRead, attach.Load, policy.Resolve)
	return report, err
}

type repairRun func(context.Context, attach.Env, *attach.World) (attach.ReconcileReport, error)

func refresh(parent context.Context, env attach.Env, mode Mode, load func(context.Context) (*attach.World, error), resolve func(context.Context, policy.Spec) (int, error)) (Image, Report, error) {
	return refreshWithRepair(parent, env, mode, load, resolve, repair)
}
func refreshWithRepair(parent context.Context, env attach.Env, mode Mode, load func(context.Context) (*attach.World, error), resolve func(context.Context, policy.Spec) (int, error), runRepair repairRun) (out Image, reportOut Report, resultErr error) {
	ctx, cancel := context.WithTimeout(parent, 1500*time.Millisecond)
	defer cancel()
	root := env.Log.Dir
	held, err := lockfile.AcquireWithin(ctx, root, "observe/refresh-lock", false, mode == Explicit)
	if errors.Is(err, lockfile.ErrBusy) {
		if mode == freshRead {
			return Image{}, Report{Skipped: "contended"}, ctx.Err()
		}
		img, readErr := ReadImage(ctx, root)
		if readErr == nil {
			state, stateErr := readState(ctx, root)
			if stateErr != nil || state.Failure {
				return Image{}, Report{}, errors.Join(stateErr, failedStateError(state))
			}
			return img, Report{Cached: true, Repair: img.Report}, nil
		}
		if mode != Bootstrap || !errors.Is(readErr, os.ErrNotExist) {
			return Image{}, Report{}, errors.Join(ErrBusy, readErr)
		}
		// An admitted subscriber may join the initial publication. Exclusion still
		// precedes every discovery, and cancellation cannot extend the shared budget.
		held, err = lockfile.AcquireWithin(ctx, root, "observe/refresh-lock", false, true)
	}
	if err != nil {
		return Image{}, Report{}, err
	}
	defer held.Close()
	s, err := readState(ctx, root)
	if err != nil {
		return Image{}, Report{}, err
	}
	// Owning the lock proves that no writer can still complete this mark.
	// Preserve the old image, record abandonment, and report uncertainty once;
	// a later eligible or explicit invocation can perform a new attempt.
	if s.InProgress {
		s.InProgress = false
		s.Failure = true
		s.FailureCode = "observation_unavailable"
		s.CompletedAt = time.Now().UTC()
		if err := save(ctx, root, "state", s); err != nil {
			return Image{}, Report{}, err
		}
		return Image{}, Report{}, ErrUnavailable
	}
	interval, err := resolve(ctx, policy.Reconcile)
	if err != nil {
		return Image{}, Report{}, err
	}
	now := time.Now().UTC()
	if mode == freshRead && interval == 0 {
		return Image{}, Report{Skipped: "disabled"}, ctx.Err()
	}
	if (mode == Opportunistic || mode == Bootstrap || mode == freshRead) && (interval == 0 || (!s.Attempt.IsZero() && !now.Before(s.Attempt) && now.Sub(s.Attempt) < time.Duration(interval)*time.Second)) {
		if s.Failure {
			return Image{}, Report{}, failedStateError(s)
		}
		if mode == freshRead {
			return Image{}, Report{Skipped: "cooldown"}, ctx.Err()
		}
		img, err := ReadImage(ctx, root)
		return img, Report{Cached: true, Repair: img.Report}, err
	}
	// Publish an active attempt without changing its predecessor's outcome.
	// A lock-busy reader may reuse a successful in-age predecessor while work
	// runs. If this writer disappears, the next lock owner detects abandonment.
	s.Attempt, s.Interval, s.InProgress = now, interval, true
	if err := save(ctx, root, "state", s); err != nil {
		return Image{}, Report{}, err
	}
	defer func() {
		if resultErr != nil && ctx.Err() == nil {
			s.InProgress = false
			s.Failure = true
			s.FailureCode = failureCode(resultErr)
			s.CompletedAt = time.Now().UTC()
			_ = save(ctx, root, "state", s)
		}
	}()
	start, err := env.Log.Head(ctx)
	if err != nil {
		return Image{}, Report{}, err
	}
	checkpoint, err := start.Checkpoint()
	if err != nil {
		return Image{}, Report{}, err
	}
	w, err := load(ctx)
	if err != nil {
		return Image{}, Report{}, err
	}
	repair, repairErr := runRepair(ctx, env, w)
	report := Report{Repair: repair}
	defer func() { recordRepair(ctx, root, resolve, repair) }()
	// Repair progress is independent of capture capacity or image readability.
	if ctx.Err() != nil {
		return Image{}, report, errors.Join(repairErr, ctx.Err())
	}
	catalog, err := env.Store.ScanBoundedContext(ctx, MaxImageBytes)
	if ctx.Err() != nil {
		return Image{}, report, errors.Join(repairErr, err, ctx.Err())
	}
	if errors.Is(err, session.ErrCatalogCapacity) {
		return Image{}, report, ErrCapacity
	}
	if err != nil || !catalog.Complete || catalog.IssueCount > 0 {
		return Image{}, report, errors.Join(repairErr, &session.RecordsUnavailable{Catalog: catalog})
	}
	records := catalog.Records
	if repairErr != nil {
		return Image{}, report, repairErr
	}
	live, _ := attach.Live(w, records)
	if s.Generation == math.MaxUint64 {
		return Image{}, report, ErrUnavailable
	}
	img := Image{Version: 1, Generation: s.Generation + 1, Created: now, Interval: interval, Start: checkpoint, Records: live, Panes: Panes(w, live), Report: repair}
	data, err := json.Marshal(img)
	if err != nil {
		return Image{}, report, err
	}
	if len(data) > MaxImageBytes {
		return Image{}, report, ErrCapacity
	}
	// Copy replay now to validate scan overlap; watchers copy it again before
	// output, so retained history cannot vanish between validation and emission.
	if _, err := env.Log.Replay(ctx, start); err != nil {
		return Image{}, report, err
	}
	if err := statefile.WriteSlots(ctx, root, "observe/image.json", "observe/.image-00", ".image-", 1, data); err != nil {
		return Image{}, report, err
	}
	s.Generation, s.Failure, s.InProgress = img.Generation, false, false
	s.FailureCode = ""
	s.SuccessAt = img.Created
	s.CompletedAt = time.Now().UTC()
	if err := save(ctx, root, "state", s); err != nil {
		return Image{}, report, err
	}
	return img, report, nil
}
func recordRepair(ctx context.Context, root string, resolve func(context.Context, policy.Spec) (int, error), report attach.ReconcileReport) {
	if report.FaultCount == 0 {
		return
	}
	enabled, err := resolve(ctx, policy.Diagnostics)
	if err != nil || enabled == 0 {
		return
	}
	for _, f := range report.Faults {
		_ = diagnostic.Record(ctx, root, diagnostic.Fault{Component: "reconcile", Operation: f.Operation, Category: f.Category, Agent: f.Agent, ID: f.ID})
	}
}

func failureCode(err error) string {
	var unavailable *session.RecordsUnavailable
	var issue *session.RecordIssue
	switch {
	case errors.As(err, &unavailable), errors.As(err, &issue):
		return "records_unavailable"
	case errors.Is(err, ErrCapacity):
		return "observation_capacity"
	case errors.Is(err, eventlog.ErrHistoryUnavailable):
		return "history_unavailable"
	}
	return "observation_unavailable"
}
func failedStateError(s refreshState) error {
	switch s.FailureCode {
	case "records_unavailable":
		return &session.RecordsUnavailable{Catalog: session.Catalog{Complete: false}}
	case "observation_capacity":
		return ErrCapacity
	case "history_unavailable":
		return &eventlog.HistoryUnavailable{Reason: "previous shared capture lost history"}
	}
	return ErrUnavailable
}
