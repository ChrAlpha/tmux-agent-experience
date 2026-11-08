package observe

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestRefreshCaptureAfterRepairBudgetDeferral(t *testing.T) {
	for _, tc := range []struct {
		name, stage  string
		fault        error
		parentCancel bool
	}{
		{name: "progress_read", stage: "read"},
		{name: "initial_progress_save", stage: "initial_save"},
		{name: "pane_catalog_scan", stage: "scan"},
		{name: "pane_progress_save", stage: "pane_save"},
		{name: "session_progress_save", stage: "session_save"},
		{name: "scan_storage_fault_and_deadline", stage: "scan", fault: io.ErrUnexpectedEOF},
		{name: "progress_storage_fault_and_deadline", stage: "pane_save", fault: io.ErrUnexpectedEOF},
		{name: "earlier_fault_survives_later_progress_deadline", stage: "prior_fault", fault: io.ErrUnexpectedEOF},
		{name: "earlier_helper_timeout_survives_own_later_expiration", stage: "prior_fault", fault: context.DeadlineExceeded},
		{name: "independent_cancellation_is_not_repair_expiration", stage: "scan", fault: context.Canceled},
		{name: "actual_parent_cancellation", stage: "scan", parentCancel: true},
		{name: "parent_cancellation_with_storage_fault", stage: "scan", parentCancel: true, fault: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env := fixture(t)
			seed, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
			if err != nil {
				t.Fatal(err)
			}
			imagePath := filepath.Join(env.Log.Dir, "observe", "image.json")
			before, err := os.ReadFile(imagePath)
			if err != nil {
				t.Fatal(err)
			}
			pending := session.Record{Agent: "codex", ID: "pending", Main: "working", Change: "UserPromptSubmit", Since: time.Unix(123, 0).UTC()}
			if err := env.Store.Put(pending); err != nil {
				t.Fatal(err)
			}
			w, err := emptyWorld(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// A non-agent pane supplies a pane-progress position without issuing any
			// tmux command. The tested deadline occurs before pane resource mutation.
			w.Panes = []tmux.Pane{{ID: "%1", Server: "unused", PID: 0}}
			ops := repairOperations(env)
			read, write, scan := ops.readProgress, ops.saveProgress, ops.scanCatalog
			reached := false
			saves := 0
			stopAtBudget := func(budget context.Context) {
				reached = true
				if tc.parentCancel {
					cancel()
				} else {
					<-budget.Done()
				}
			}
			ops.readProgress = func(budget context.Context) (progress, error) {
				if tc.stage == "read" {
					stopAtBudget(budget)
				}
				p, err := read(budget)
				if tc.stage == "session_save" {
					p.First = 1
				} else {
					p.First = 2
				}
				return p, err
			}
			ops.saveProgress = func(budget context.Context, p progress) error {
				saves++
				target := tc.stage == "initial_save" && saves == 1 || (tc.stage == "pane_save" || tc.stage == "session_save" || tc.stage == "prior_fault") && saves == 2
				if !target {
					return write(budget, p)
				}
				stopAtBudget(budget)
				// Preserve a real fault joined with the same deadline. In prior_fault,
				// the I/O error came from the preceding phase, not this save.
				if tc.stage == "prior_fault" {
					return write(budget, p)
				}
				return errors.Join(write(budget, p), tc.fault)
			}
			ops.scanCatalog = func(budget context.Context) (session.Catalog, error) {
				if tc.stage == "prior_fault" {
					return session.Catalog{}, tc.fault
				}
				if tc.stage != "scan" {
					return scan(budget)
				}
				stopAtBudget(budget)
				catalog, err := scan(budget)
				return catalog, errors.Join(err, tc.fault)
			}
			run := func(parent context.Context, env attach.Env, w *attach.World) (attach.ReconcileReport, error) {
				return repairUsing(parent, env, w, ops)
			}
			load := func(ctx context.Context) (*attach.World, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return w, nil
			}
			img, report, err := refreshWithRepair(ctx, env, Explicit, load, defaults, run)
			if !reached {
				t.Fatal("fixture did not reach its selected repair I/O boundary")
			}
			if report.Repair.Complete || report.Repair.Deferred == 0 {
				t.Fatalf("missing bounded deferral: %+v", report.Repair)
			}
			switch {
			case tc.parentCancel:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("parent cancellation lost: %v", err)
				}
				if tc.fault != nil && !errors.Is(err, tc.fault) {
					t.Fatalf("parent cancellation masked joined storage fault: %v", err)
				}
				if tc.fault != nil && report.Repair.FaultCount == 0 {
					t.Fatal("joined storage fault misclassified as cancellation only")
				}
			case tc.fault != nil:
				if !errors.Is(err, tc.fault) || errors.Is(err, context.DeadlineExceeded) != errors.Is(tc.fault, context.DeadlineExceeded) {
					t.Fatalf("real branch lost or own deadline escaped: %v", err)
				}
				if report.Repair.FaultCount == 0 {
					t.Fatal("real fault misclassified as mere deferral")
				}
			default:
				if err != nil || img.Generation != seed.Generation+1 || report.Repair.FaultCount != 0 || len(img.Records) != 1 {
					t.Fatalf("healthy capture after deferral: image=%+v report=%+v err=%v", img, report, err)
				}
				start, err := img.Start.Cursor()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := env.Log.Replay(ctx, start); err != nil {
					t.Fatal("capture lost replay validation", err)
				}
				state, err := readState(ctx, env.Log.Dir)
				if err != nil || state.InProgress || state.Failure {
					t.Fatalf("healthy deferred capture recorded failure: %+v %v", state, err)
				}
			}
			if tc.fault != nil || tc.parentCancel {
				after, readErr := os.ReadFile(imagePath)
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("failed/canceled capture replaced image: %v", readErr)
				}
			}
			// The publication marker is the durable repair queue. A budget return may
			// not erase it to make image capture appear successful.
			got, found, err := env.Store.Get(pending.Agent, pending.ID)
			if err != nil || !found || !reflect.DeepEqual(got, pending) || !got.Unsettled() {
				t.Fatalf("pending state was changed or lost: %+v %v", got, err)
			}
		})
	}
}

func TestRepairAndCapturePreserveActualParentDeadline(t *testing.T) {
	env := fixture(t)
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	w, err := emptyWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report, err := repair(expired, env, w)
	if !errors.Is(err, context.DeadlineExceeded) || report.Complete {
		t.Fatalf("repair masked parent timeout: %+v %v", report, err)
	}
	if _, _, err := refresh(expired, env, Explicit, emptyWorld, defaults); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture masked parent timeout: %v", err)
	}
}
