package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
)

func TestAdmissionGateContentionIsNotCapacity(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	held, err := lockfile.AcquireWithin(ctx, root, "coord/admission-lock", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if held != nil {
			_ = held.Close()
		}
	}()
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	resolved := false
	l, err := acquire(short, root, Watch, func(ctx context.Context, s policy.Spec) (int, error) { resolved = true; return s.Default, ctx.Err() })
	if l != nil {
		defer l.Close()
		t.Fatal("admitted through another gate holder")
	}
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrLimit) || !errors.Is(err, context.DeadlineExceeded) || resolved {
		t.Fatalf("contention fabricated capacity: resolved=%t err=%v", resolved, err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	held = nil
	first, err := acquire(ctx, root, Watch, limit(1))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := acquire(ctx, root, Watch, limit(1)); second != nil || !errors.Is(err, ErrLimit) || errors.Is(err, ErrUnavailable) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("real full capacity misclassified: %v", err)
	}
}
func TestConcurrentBelowLimitAcquisitionUsesDistinctLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	root := t.TempDir()
	ready := make(chan struct{})
	type result struct {
		lease *Lease
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { <-ready; l, err := acquire(ctx, root, Watch, limit(2)); results <- result{l, err} }()
	}
	close(ready)
	var leases []*Lease
	defer func() {
		for _, l := range leases {
			_ = l.Close()
		}
	}()
	for range 2 {
		r := <-results
		if r.lease != nil {
			leases = append(leases, r.lease)
		}
		if r.err != nil {
			t.Errorf("below-limit simultaneous startup: %v", r.err)
		}
	}
	if len(leases) != 2 || leases[0].Slot() == leases[1].Slot() {
		t.Fatalf("did not admit two distinct live slots: %d", len(leases))
	}
	if over, err := acquire(ctx, root, Watch, limit(2)); over != nil || !errors.Is(err, ErrLimit) {
		if over != nil {
			_ = over.Close()
		}
		t.Fatalf("concurrent acquisition lost capacity accounting: %v", err)
	}
}
