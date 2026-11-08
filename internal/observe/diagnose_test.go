package observe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
)

func TestDiagnosisShowsFailureRecoveryAndKeepsHistory(t *testing.T) {
	ctx := context.Background()
	env := fixture(t)
	seed, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
	if err != nil {
		t.Fatal(err)
	}
	fault := diagnostic.Fault{Component: "observe", Operation: "refresh", Category: "io", Agent: "codex", ID: "thread"}
	if err := diagnostic.Record(ctx, env.Log.Dir, fault); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	close(release)
	barrier := loadBarrier{entered: make(chan struct{}), release: release, failure: errors.New("private failure sentinel")}
	if _, _, err := refresh(ctx, env, Explicit, barrier.load, defaults); err == nil {
		t.Fatal("failure fixture succeeded")
	}
	before, err := os.ReadFile(filepath.Join(env.Log.Dir, "observe", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	failed := Diagnose(ctx, env.Log.Dir)
	if failed.LastAttempt.Outcome != "completed_failure" || failed.LastAttempt.CompletedAt == nil || failed.LastSuccess == nil || failed.LastSuccess.Generation != seed.Generation || failed.LastSuccess.At == nil {
		t.Fatalf("missing failure evidence: %+v", failed)
	}
	encoded, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private failure sentinel") {
		t.Fatal("raw error exposed")
	}
	after, err := os.ReadFile(filepath.Join(env.Log.Dir, "observe", "state.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("diagnosis changed attempt state", err)
	}
	recovered, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
	if err != nil {
		t.Fatal(err)
	}
	healthy := Diagnose(ctx, env.Log.Dir)
	history := diagnostic.Probe(ctx, env.Log.Dir, 16)
	if healthy.LastAttempt.Outcome != "succeeded" || healthy.LastSuccess == nil || healthy.LastSuccess.Generation != recovered.Generation || history.RecordedTotal != 1 {
		t.Fatalf("recovery discarded history or retained current failure: %+v %+v", healthy, history)
	}
}

func TestDiagnosisSeparatesActiveMarkerAndIncompleteEvidence(t *testing.T) {
	ctx := context.Background()
	env := fixture(t)
	if _, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults); err != nil {
		t.Fatal(err)
	}
	holder, err := lockfile.AcquireWithin(ctx, env.Log.Dir, "observe/refresh-lock", false, false)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readState(ctx, env.Log.Dir)
	if err != nil {
		t.Fatal(err)
	}
	state.InProgress = true
	state.Attempt = time.Now().UTC()
	if err := save(ctx, env.Log.Dir, "state", state); err != nil {
		t.Fatal(err)
	}
	active := Diagnose(ctx, env.Log.Dir)
	if active.Lock != "held" || active.LastAttempt.Outcome != "active_or_contended" || active.LastAttempt.CompletedAt != nil {
		t.Fatalf("active evidence misreported: %+v", active)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	incomplete := Diagnose(ctx, env.Log.Dir)
	if incomplete.Lock != "free" || incomplete.LastAttempt.Outcome != "incomplete" {
		t.Fatalf("marker alone proved worker liveness: %+v", incomplete)
	}
	unchanged, err := readState(ctx, env.Log.Dir)
	if err != nil || !unchanged.InProgress || unchanged.Failure {
		t.Fatal("diagnosis repaired/acknowledged incomplete marker", err)
	}
}

func TestDiagnosisShowsStaleUnreadableAndNeverObserved(t *testing.T) {
	ctx := context.Background()
	env := fixture(t)
	never := Diagnose(ctx, filepath.Join(t.TempDir(), "not-created"))
	if never.LastAttempt.Outcome != "never_observed" || never.LastSuccess != nil {
		t.Fatalf("invented observation: %+v", never)
	}
	img, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
	if err != nil {
		t.Fatal(err)
	}
	img.Created = time.Now().Add(-time.Hour)
	if err := save(ctx, env.Log.Dir, "image", img); err != nil {
		t.Fatal(err)
	}
	if got := Diagnose(ctx, env.Log.Dir); got.Image != "stale" {
		t.Fatalf("stale image hidden: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(env.Log.Dir, "observe", "image.json"), []byte("private malformed metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Diagnose(ctx, env.Log.Dir); got.Image != "unreadable" || got.Complete {
		t.Fatalf("unreadable image called healthy: %+v", got)
	}
}
