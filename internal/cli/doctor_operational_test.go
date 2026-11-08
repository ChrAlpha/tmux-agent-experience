package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/maintenance"
	"github.com/ChrAlpha/tmux-agent-experience/internal/observe"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

func doctorDefaults(ctx context.Context, s policy.Spec) (int, error) { return s.Default, ctx.Err() }
func doctorFixture(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	out := &bytes.Buffer{}
	return &app{log: eventlog.Open(root), stdout: out, stderr: &bytes.Buffer{}}, out
}
func doctorObservation(t *testing.T, a *app, generation uint64, failed bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	head, err := a.log.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start, err := head.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	img := observe.Image{Version: 1, Generation: generation, Created: now, Interval: 2, Start: start, Records: []session.Record{}, Panes: []observe.Pane{}}
	imageData, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteSlots(ctx, a.log.Dir, "observe/image.json", "observe/.image-00", ".image-", 1, imageData); err != nil {
		t.Fatal(err)
	}
	code := ""
	if failed {
		code = "records_unavailable"
	}
	stateData, err := json.Marshal(map[string]any{"Version": 1, "Attempt": now, "Generation": generation, "Interval": 2, "Failure": failed, "FailureCode": code, "SuccessAt": now, "CompletedAt": now})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteSlots(ctx, a.log.Dir, "observe/state.json", "observe/.state-00", ".state-", 1, stateData); err != nil {
		t.Fatal(err)
	}
	held, err := lockfile.AcquireWithin(ctx, a.log.Dir, "observe/refresh-lock", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}
func namedCheck(checks []check, name string) check {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return check{Name: name, Status: "missing"}
}
func TestDoctorShowsKnownFailuresAndRecoveryRetainsHistory(t *testing.T) {
	a, out := doctorFixture(t)
	ctx := context.Background()
	doctorObservation(t, a, 1, true)
	if err := diagnostic.Record(ctx, a.log.Dir, diagnostic.Fault{Component: "observe", Operation: "refresh", Category: "io", Agent: "codex", ID: "thread"}); err != nil {
		t.Fatal(err)
	}
	pending := session.Record{Agent: "codex", ID: "thread", Main: "idle", Sent: time.Now().UTC().Add(-time.Hour)}
	pending.Observe(time.Now().UTC(), "", true)
	if err := a.store().Put(pending); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(a.log.Dir, "sessions", "codex", "broken.json")
	if err := os.WriteFile(corrupt, []byte("private decoder/notification sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Run(ctx, a.log.Dir); err != nil {
		t.Fatal(err)
	}
	failed := a.probeOperational(ctx, doctorDefaults)
	checks := operationalChecks(failed)
	if failed.History.RecordedTotal != 1 || len(failed.History.Buckets) != 1 || failed.History.Buckets[0].First.IsZero() || failed.History.Buckets[0].Last.IsZero() || failed.Observation.LastAttempt.Outcome != "completed_failure" || failed.Observation.LastSuccess == nil || failed.Records.KnownFaults == 0 || namedCheck(checks, "records").Status != "fail" {
		t.Fatalf("actionable failure missing: %+v", failed)
	}
	if failed.Records.UnconfirmedTotal != 1 || failed.Records.Unconfirmed[0].Status != "unconfirmed" {
		t.Fatalf("send confirmation evidence missing: %+v", failed.Records)
	}
	a.printOperational(failed)
	if !strings.Contains(out.String(), "observe/refresh/io") || !strings.Contains(out.String(), "first=") || !strings.Contains(out.String(), "last=") || !strings.Contains(out.String(), "unconfirmed") {
		t.Fatalf("human detail missing: %s", out.String())
	}
	encoded, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("private decoder/notification sentinel")) {
		t.Fatal("raw content escaped diagnostics")
	}
	if err := os.Remove(corrupt); err != nil {
		t.Fatal(err)
	}
	doctorObservation(t, a, 2, false)
	recovered := a.probeOperational(ctx, doctorDefaults)
	checks = operationalChecks(recovered)
	if recovered.Records.KnownFaults != 0 || !recovered.Records.Complete || recovered.Observation.LastAttempt.Outcome != "succeeded" || recovered.History.RecordedTotal != 1 || namedCheck(checks, "diagnostics").Status != "ok" || namedCheck(checks, "records").Status != "ok" {
		t.Fatalf("old history became current failure after repair: %+v", recovered)
	}
	rec, found, err := a.store().Get("codex", "thread")
	if err != nil || !found || !rec.Sent.Equal(pending.Sent) || rec.State() != "idle" {
		t.Fatal("doctor changed delivery/state evidence", err)
	}
}
func TestDoctorFloodOutputIsBoundedAndReportsOverflow(t *testing.T) {
	a, out := doctorFixture(t)
	ctx := context.Background()
	doctorObservation(t, a, 1, false)
	components := []string{"observe", "hook", "maintenance"}
	operations := []string{"run", "scan", "refresh", "session"}
	categories := []string{"io", "decode", "identity", "permission", "unsafe_object", "failed", "timeout"}
	for i := range 80 {
		fault := diagnostic.Fault{Component: components[i%len(components)], Operation: operations[(i/len(components))%len(operations)], Category: categories[(i/(len(components)*len(operations)))%len(categories)], Agent: "codex", ID: fmt.Sprintf("thread-%d", i)}
		if err := diagnostic.Record(ctx, a.log.Dir, fault); err != nil {
			t.Fatal(err)
		}
	}
	e := a.probeOperational(ctx, doctorDefaults)
	if e.History.BucketsTotal != diagnostic.MaxBuckets || e.History.RecordedTotal != 80 || e.History.Overflow == 0 || !e.History.Truncated || len(e.History.Buckets) != doctorSampleLimit {
		t.Fatalf("flood bound/overflow hidden: %+v", e.History)
	}
	a.printOperational(e)
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 24<<10 || out.Len() > 16<<10 {
		t.Fatalf("unbounded output: JSON=%d human=%d", len(data), out.Len())
	}
}
func TestDoctorUncertaintyRecordingOffAndBoundedRecords(t *testing.T) {
	a, _ := doctorFixture(t)
	ctx := context.Background()
	doctorObservation(t, a, 1, false)
	if err := maintenance.Run(ctx, a.log.Dir); err != nil {
		t.Fatal(err)
	}
	if err := diagnostic.Record(ctx, a.log.Dir, diagnostic.Fault{Component: "hook", Operation: "run", Category: "failed"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(a.log.Dir, ".tmag-scope.json"))
	if err != nil {
		t.Fatal(err)
	}
	off := func(ctx context.Context, s policy.Spec) (int, error) {
		if s == policy.Diagnostics {
			return 0, ctx.Err()
		}
		return s.Default, ctx.Err()
	}
	e := a.probeOperational(ctx, off)
	if e.Recording != "off" || e.Maintenance.Outcome != "unknown" || e.History.RecordedTotal != 1 {
		t.Fatalf("off treated missing/current evidence as success: %+v", e)
	}
	after, err := os.ReadFile(filepath.Join(a.log.Dir, ".tmag-scope.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("doctor rewrote ownership manifest", err)
	}
	if err := os.WriteFile(filepath.Join(a.log.Dir, "diagnostics", "summary.json"), []byte("private bad summary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.log.Dir, "observe", "image.json"), []byte("private bad image"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := a.probeOperational(ctx, doctorDefaults)
	if unknown.History.State != "unreadable" || unknown.Observation.Image != "unreadable" || unknown.Complete {
		t.Fatalf("corruption became success: %+v", unknown)
	}
	if err := a.store().Put(session.Record{Agent: "codex", ID: "healthy", Main: "idle"}); err != nil {
		t.Fatal(err)
	}
	for i := range doctorRecordLimit + 10 {
		if err := os.WriteFile(filepath.Join(a.log.Dir, "sessions", "codex", fmt.Sprintf("unknown-%03d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bounded := a.probeOperational(ctx, doctorDefaults)
	if bounded.Records.Complete || !bounded.Records.Truncated || bounded.Records.KnownFaults != 0 || namedCheck(operationalChecks(bounded), "records").Status != "warn" {
		t.Fatalf("entry cap became false completeness/fault: %+v", bounded.Records)
	}
}
