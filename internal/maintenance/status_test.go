package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

func enabled(ctx context.Context, s policy.Spec) (int, error) { return s.Default, ctx.Err() }
func steadyWorkers() [3]worker {
	w := func(ctx context.Context) (Work, error) { return Work{Examined: 2, Removed: 1}, ctx.Err() }
	return [3]worker{w, w, w}
}
func TestMaintenanceProgressFailureRecoveryAndLegacyCursor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := statefile.WriteWithin(ctx, root, "coord/maintenance", "coord/maintenance.tmp", []byte("2\n")); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, root, enabled, steadyWorkers()); err != nil {
		t.Fatal(err)
	}
	status := Probe(ctx, root, "on")
	if status.State != "readable" || status.Outcome != "completed" || status.Status == nil || status.Status.Admitted != 1 || status.Status.Completed != 1 || status.Status.LastCompleted.Work.Examined != 6 || status.Status.LastCompleted.Work.Removed != 3 {
		t.Fatalf("missing progress: %+v", status)
	}
	cursor, err := os.ReadFile(filepath.Join(root, "coord", "maintenance"))
	if err != nil || string(cursor) != "0\n" {
		t.Fatalf("legacy cursor changed format/rotation: %q %v", cursor, err)
	}
	failedWorkers := steadyWorkers()
	failure := errors.New("private maintenance failure")
	failedWorkers[0] = func(ctx context.Context) (Work, error) { return Work{Examined: 1}, errors.Join(failure, ctx.Err()) }
	if err := run(ctx, root, enabled, failedWorkers); !errors.Is(err, failure) {
		t.Fatalf("real failure hidden: %v", err)
	}
	status = Probe(ctx, root, "on")
	if status.Outcome != "failed" || status.Status.LastCompleted.Failure != "io" {
		t.Fatalf("persistent failure hidden: %+v", status)
	}
	history := diagnostic.Probe(ctx, root, 16)
	if history.RecordedTotal != 1 {
		t.Fatalf("maintenance category missing: %+v", history)
	}
	if err := run(ctx, root, enabled, steadyWorkers()); err != nil {
		t.Fatal(err)
	}
	recovered := Probe(ctx, root, "on")
	if recovered.Outcome != "completed" || recovered.Status.Completed != 3 || diagnostic.Probe(ctx, root, 16).RecordedTotal != 1 {
		t.Fatalf("recovery failed to preserve history: %+v", recovered)
	}
}
func TestMaintenanceRecordingOffAndMissingEvidenceStayUnknown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	off := func(ctx context.Context, _ policy.Spec) (int, error) { return 0, ctx.Err() }
	if err := run(ctx, root, off, steadyWorkers()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "coord", "maintenance-status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("off wrote progress history", err)
	}
	if got := Probe(ctx, root, "off"); got.Outcome != "unknown" || got.Status != nil {
		t.Fatalf("off became false success: %+v", got)
	}
	if err := run(ctx, root, enabled, steadyWorkers()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "coord", "maintenance-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, root, off, steadyWorkers()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "coord", "maintenance-status.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("off rewrote historical status", err)
	}
	if got := Probe(ctx, root, "off"); got.Outcome != "unknown" || got.Status == nil {
		t.Fatalf("retained history misreported: %+v", got)
	}
}
func TestMaintenanceIncompleteAndUnreadableStatusAreNotSuccess(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ticket, err := admitted(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := Probe(ctx, root, "on"); got.Outcome != "incomplete_evidence" {
		t.Fatalf("marker claimed completed/live work: %+v", got)
	}
	if err := completed(ctx, root, ticket, Work{Deferrals: 2, Classes: 1}, "deferred", ""); err != nil {
		t.Fatal(err)
	}
	if got := Probe(ctx, root, "on"); got.Outcome != "deferred" || got.Status.LastCompleted.Work.Deferrals != 2 {
		t.Fatalf("deferrals hidden: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(root, "coord", "maintenance-status.json"), []byte("private malformed status"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Probe(ctx, root, "on"); got.State != "unreadable" || got.Outcome != "unknown" {
		t.Fatalf("bad status called success: %+v", got)
	}
}

func TestInvalidLegacyCursorIsVisibleAsKnownMaintenanceFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := statefile.WriteWithin(ctx, root, "coord/maintenance", "coord/maintenance.tmp", []byte("invalid")); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, root, enabled, steadyWorkers()); err == nil {
		t.Fatal("invalid cursor succeeded")
	}
	got := Probe(ctx, root, "on")
	if got.State != "readable" || got.Outcome != "failed" || got.Status.LastCompleted.Work.Classes != 0 {
		t.Fatalf("admitted failure before work hidden: %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "coord", "maintenance"))
	if err != nil || string(data) != "invalid" {
		t.Fatal("failed status publication rewrote legacy cursor", err)
	}
}
