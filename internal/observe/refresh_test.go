package observe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func fixture(t *testing.T) attach.Env {
	t.Helper()
	root := t.TempDir()
	return attach.Env{Log: eventlog.Open(root), Store: session.Store{Dir: root}, Snapshot: func(context.Context) (*proc.Table, error) { return proc.NewTable(nil), nil }}
}
func defaults(_ context.Context, s policy.Spec) (int, error) { return s.Default, nil }
func emptyWorld(context.Context) (*attach.World, error) {
	return &attach.World{Proc: proc.NewTable(nil)}, nil
}

type loadBarrier struct {
	entered chan struct{}
	release <-chan struct{}
	failure error
	loads   *atomic.Int32
}

func (b loadBarrier) load(ctx context.Context) (*attach.World, error) {
	if b.loads != nil {
		b.loads.Add(1)
	}
	close(b.entered)
	select {
	case <-b.release:
		if b.failure != nil {
			return nil, b.failure
		}
		return emptyWorld(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentRefreshExcludesDiscoveryAndCachesOneImage(t *testing.T) {
	env := fixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var loads atomic.Int32
	load := func(ctx context.Context) (*attach.World, error) {
		loads.Add(1)
		close(entered)
		select {
		case <-release:
			return emptyWorld(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	go func() { _, _, err := refresh(ctx, env, Opportunistic, load, defaults); done <- err }()
	<-entered
	if _, _, err := refresh(ctx, env, Opportunistic, load, defaults); !errors.Is(err, ErrBusy) {
		t.Fatalf("contender=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	img, report, err := refresh(ctx, env, Opportunistic, load, defaults)
	if err != nil || !report.Cached || img.Generation != 1 || loads.Load() != 1 {
		t.Fatalf("img=%+v report=%+v scans=%d err=%v", img, report, loads.Load(), err)
	}
	// The image checkpoint predates later mutations, preserving their replay.
	if err := env.Log.Append(ctx, &eventlog.Event{Agent: "codex", Session: "new", Event: "Observation", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	start, err := img.Start.Cursor()
	if err != nil {
		t.Fatal(err)
	}
	batch, err := env.Log.Replay(ctx, start)
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("backlog=%v err=%v", batch, err)
	}
}

func TestFailedRefreshPreservesImageAndPendingRepair(t *testing.T) {
	env := fixture(t)
	ctx := context.Background()
	if _, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(env.Log.Dir, "observe", "image.json")
	before, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Put(session.Record{Agent: "codex", ID: "healthy", Main: "idle", Change: "Stop"}); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(env.Log.Dir, "sessions", "codex", "broken.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults); err == nil {
		t.Fatal("corrupt image succeeded")
	}
	after, err := os.ReadFile(imagePath)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed pass replaced last valid image", err)
	}
	r, found, err := env.Store.Get("codex", "healthy")
	if err != nil || !found || r.Unsettled() {
		t.Fatalf("valid repair did not progress: %+v %v", r, err)
	}
	if data, err := os.ReadFile(corrupt); err != nil || string(data) != "{" {
		t.Fatal("damaged original changed", err)
	}
	var unavailable *session.RecordsUnavailable
	if _, _, err := refresh(ctx, env, Opportunistic, emptyWorld, defaults); !errors.As(err, &unavailable) {
		t.Fatalf("failure hidden by cached image: %v", err)
	}
}

func TestRepairResumesBeyondOneBatch(t *testing.T) {
	env := fixture(t)
	ctx := context.Background()
	for i := range repairLimit + 40 {
		if err := env.Store.Put(session.Record{Agent: "codex", ID: fmt.Sprintf("thread-%03d", i), Main: "idle", Change: "Stop"}); err != nil {
			t.Fatal(err)
		}
	}
	for pass := range 64 {
		_, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
		if err != nil {
			t.Fatal(err)
		}
		records, err := env.Store.All()
		if err != nil {
			t.Fatal(err)
		}
		pending := 0
		for _, r := range records {
			if r.Unsettled() {
				pending++
			}
		}
		if pending == 0 {
			return
		}
		if pass == 63 {
			t.Fatalf("repair starved %d records", pending)
		}
	}
}
func TestStaleImageAndCanceledRefreshFailExplicitly(t *testing.T) {
	env := fixture(t)
	ctx := context.Background()
	img, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
	if err != nil {
		t.Fatal(err)
	}
	img.Created = time.Now().Add(-time.Hour)
	if err := save(ctx, env.Log.Dir, "image", img); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadImage(ctx, env.Log.Dir); !errors.Is(err, ErrUnavailable) {
		t.Fatal("stale image accepted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := refresh(canceled, env, Explicit, emptyWorld, defaults); err == nil {
		t.Fatal("canceled refresh succeeded")
	}
}

func TestActiveRefreshReusesSeededImageWithoutAnotherDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := fixture(t)
	var loads atomic.Int32
	seedLoad := func(ctx context.Context) (*attach.World, error) { loads.Add(1); return emptyWorld(ctx) }
	seed, _, err := refresh(ctx, env, Explicit, seedLoad, defaults)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	barrier := loadBarrier{entered: entered, release: release, loads: &loads}
	go func() { _, _, err := refresh(ctx, env, Explicit, barrier.load, defaults); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("second refresh never entered discovery")
	}
	state, err := readState(ctx, env.Log.Dir)
	if err != nil || !state.InProgress || state.Failure {
		t.Fatalf("active attempt misclassified: %+v %v", state, err)
	}
	for range 3 {
		cached, report, err := refresh(ctx, env, Opportunistic, seedLoad, defaults)
		if err != nil || !report.Cached || cached.Generation != seed.Generation || cached.Start != seed.Start {
			t.Fatalf("healthy contender failed or changed checkpoint: generation=%d report=%+v err=%v", cached.Generation, report, err)
		}
	}
	if loads.Load() != 2 {
		t.Fatalf("discovery scans=%d; want seed + one holder", loads.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	latest, report, err := refresh(ctx, env, Opportunistic, seedLoad, defaults)
	if err != nil || !report.Cached || latest.Generation != seed.Generation+1 || loads.Load() != 2 {
		t.Fatalf("completed image=%+v report=%+v scans=%d err=%v", latest, report, loads.Load(), err)
	}
	state, err = readState(ctx, env.Log.Dir)
	if err != nil || state.InProgress || state.Failure {
		t.Fatalf("successful completion not recorded: %+v %v", state, err)
	}
}

func TestActiveRefreshFailureDoesNotCertifyPreviousImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := fixture(t)
	seed, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	barrier := loadBarrier{entered: entered, release: release, failure: &session.RecordsUnavailable{Catalog: session.Catalog{Complete: false}}}
	go func() { _, _, err := refresh(ctx, env, Explicit, barrier.load, defaults); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("second refresh never entered discovery")
	}
	if img, report, err := refresh(ctx, env, Opportunistic, emptyWorld, defaults); err != nil || !report.Cached || img.Generation != seed.Generation {
		t.Fatalf("active healthy predecessor not reusable: %+v %v", report, err)
	}
	close(release)
	var unavailable *session.RecordsUnavailable
	if err := <-done; !errors.As(err, &unavailable) {
		t.Fatalf("holder failure=%v", err)
	}
	state, err := readState(ctx, env.Log.Dir)
	if err != nil || state.InProgress || !state.Failure || state.FailureCode != "records_unavailable" {
		t.Fatalf("failed completion not recorded: %+v %v", state, err)
	}
	if _, _, err := refresh(ctx, env, Opportunistic, emptyWorld, defaults); !errors.As(err, &unavailable) {
		t.Fatalf("completed failure hidden by old image: %v", err)
	}
	if img, err := readImage(ctx, env.Log.Dir); err != nil || img.Generation != seed.Generation {
		t.Fatalf("failed attempt replaced image: %+v %v", img, err)
	}
}

func TestAbandonedRefreshDetectedAfterRegainingExclusion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := fixture(t)
	seed, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults)
	if err != nil {
		t.Fatal(err)
	}
	attempt, stop := context.WithCancel(ctx)
	defer stop()
	entered := make(chan struct{})
	done := make(chan error, 1)
	barrier := loadBarrier{entered: entered}
	go func() { _, _, err := refresh(attempt, env, Explicit, barrier.load, defaults); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("second refresh never entered discovery")
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled holder=%v", err)
	}
	// Cancellation cannot extend its deadline to clear the durable marker.
	state, err := readState(ctx, env.Log.Dir)
	if err != nil || !state.InProgress || state.Failure {
		t.Fatalf("missing abandoned-attempt evidence: %+v %v", state, err)
	}
	var loads atomic.Int32
	load := func(ctx context.Context) (*attach.World, error) { loads.Add(1); return emptyWorld(ctx) }
	if _, _, err := refresh(ctx, env, Opportunistic, load, defaults); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("abandoned attempt certified old image: %v", err)
	}
	state, err = readState(ctx, env.Log.Dir)
	if err != nil || state.InProgress || !state.Failure || loads.Load() != 0 {
		t.Fatalf("abandonment did not settle separately from discovery: %+v scans=%d err=%v", state, loads.Load(), err)
	}
	// A later explicit invocation can recover, without losing the prior image
	// generation, checkpoint validation or any pending resource repair marker.
	recovered, _, err := refresh(ctx, env, Explicit, load, defaults)
	if err != nil || recovered.Generation != seed.Generation+1 || loads.Load() != 1 {
		t.Fatalf("recovery generation=%d scans=%d err=%v", recovered.Generation, loads.Load(), err)
	}
}

func TestBootstrapSharesUnseededPublicationWithinExistingBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	env := fixture(t)
	var loads atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	barrier := loadBarrier{entered: entered, release: release, loads: &loads}
	go func() { _, _, err := refresh(ctx, env, Bootstrap, barrier.load, defaults); holder <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("initial publication never entered discovery")
	}
	// With no seeded image, an opportunistic surface still refuses promptly.
	if _, _, err := refresh(ctx, env, Opportunistic, emptyWorld, defaults); !errors.Is(err, ErrBusy) {
		t.Fatalf("unseeded opportunistic contention: %v", err)
	}
	// An admitted subscriber waits for the existing publication, but its own
	// earlier parent deadline must end that wait without entering discovery.
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	_, _, err := refresh(short, env, Bootstrap, emptyWorld, defaults)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || loads.Load() != 1 {
		t.Fatalf("bootstrap wait lost deadline or duplicated discovery: loads=%d err=%v", loads.Load(), err)
	}
	type result struct {
		image  Image
		report Report
		err    error
	}
	joined := make(chan result, 1)
	go func() {
		img, report, err := refresh(ctx, env, Bootstrap, emptyWorld, defaults)
		joined <- result{img, report, err}
	}()
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	r := <-joined
	if r.err != nil || !r.report.Cached || r.image.Generation != 1 || loads.Load() != 1 {
		t.Fatalf("bootstrap failed to share publication: generation=%d cached=%t loads=%d err=%v", r.image.Generation, r.report.Cached, loads.Load(), r.err)
	}
	start, err := r.image.Start.Cursor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Log.Replay(ctx, start); err != nil {
		t.Fatal("joined image lost checkpoint validation", err)
	}
}

func TestOrdinaryTriggersMakeRepairProgressAfterCooldown(t *testing.T) {
	ctx := context.Background()
	env := fixture(t)
	loads := 0
	load := func(ctx context.Context) (*attach.World, error) { loads++; return emptyWorld(ctx) }
	seed, _, err := refresh(ctx, env, Explicit, load, defaults)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Unix(12, 345).UTC()
	pending := session.Record{Agent: "claude", ID: "pending", Main: "idle", Since: since, Change: "Stop"}
	if err := env.Store.Put(pending); err != nil {
		t.Fatal(err)
	}
	if _, r, err := refresh(ctx, env, Opportunistic, load, defaults); err != nil || !r.Cached || loads != 1 {
		t.Fatalf("cooldown did extra global discovery: loads=%d cached=%t err=%v", loads, r.Cached, err)
	}
	current, found, err := env.Store.Get(pending.Agent, pending.ID)
	if err != nil || !found || !current.Unsettled() {
		t.Fatal("fixture was already settled during cooldown", err)
	}
	// Advance eligibility deterministically rather than sleeping or forcing a
	// maintenance mode. The ordinary mode must do one bounded shared pass.
	state, err := readState(ctx, env.Log.Dir)
	if err != nil {
		t.Fatal(err)
	}
	state.Attempt = time.Now().Add(-time.Duration(state.Interval+1) * time.Second)
	if err := save(ctx, env.Log.Dir, "state", state); err != nil {
		t.Fatal(err)
	}
	next, _, err := refresh(ctx, env, Opportunistic, load, defaults)
	if err != nil || loads != 2 || next.Generation != seed.Generation+1 {
		t.Fatalf("ordinary eligible trigger did not progress: loads=%d generation=%d err=%v", loads, next.Generation, err)
	}
	current, found, err = env.Store.Get(pending.Agent, pending.ID)
	if err != nil || !found || current.Unsettled() || !current.Since.Equal(since) {
		t.Fatalf("repair lost state/marker semantics: %+v %v", current, err)
	}
	events, _, err := env.Log.Since(ctx, 0)
	if err != nil || len(events) != 1 || events[0].Event != "Stop" || events[0].Since == nil || !events[0].Since.Equal(since) {
		t.Fatalf("repair event mismatch: %+v %v", events, err)
	}
	if _, r, err := refresh(ctx, env, Opportunistic, load, defaults); err != nil || !r.Cached || loads != 2 {
		t.Fatalf("subsequent observer bypassed coalescing: loads=%d cached=%t err=%v", loads, r.Cached, err)
	}
}

func TestFreshReadTriggerSkipsDisabledOrCooldownWithoutImageDependency(t *testing.T) {
	ctx := context.Background()
	env := fixture(t)
	disabled := func(ctx context.Context, s policy.Spec) (int, error) {
		if s == policy.Reconcile {
			return 0, ctx.Err()
		}
		return s.Default, ctx.Err()
	}
	noDiscovery := func(ctx context.Context) (*attach.World, error) {
		t.Error("skipped optional repair ran global discovery")
		return emptyWorld(ctx)
	}
	_, report, err := refresh(ctx, env, freshRead, noDiscovery, disabled)
	if err != nil || report.Skipped != "disabled" {
		t.Fatalf("disabled repair required a missing cache: %+v %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(env.Log.Dir, "observe", "image.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled repair created a shared image", err)
	}
	if _, _, err := refresh(ctx, env, Explicit, emptyWorld, defaults); err != nil {
		t.Fatal(err)
	}
	img, err := readImage(ctx, env.Log.Dir)
	if err != nil {
		t.Fatal(err)
	}
	img.Created = time.Now().Add(-time.Hour)
	if err := save(ctx, env.Log.Dir, "image", img); err != nil {
		t.Fatal(err)
	}
	_, report, err = refresh(ctx, env, freshRead, noDiscovery, defaults)
	if err != nil || report.Skipped != "cooldown" {
		t.Fatalf("fresh read required an expired image during cooldown: %+v %v", report, err)
	}
	// Shared-image consumers retain their explicit stale-image refusal.
	if _, _, err := refresh(ctx, env, Opportunistic, noDiscovery, defaults); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("image contract was weakened: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.Log.Dir, "observe", "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := refresh(ctx, env, freshRead, noDiscovery, disabled); err == nil {
		t.Fatal("disabled repair hid malformed metadata")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := refresh(canceled, env, freshRead, noDiscovery, disabled); !errors.Is(err, context.Canceled) {
		t.Fatalf("skipped repair hid parent cancellation: %v", err)
	}
}
