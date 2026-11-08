package admission

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
)

func limit(n int) resolver { return func(context.Context, policy.Spec) (int, error) { return n, nil } }

func TestLeasesCountBothStartupAndDrainAndLoweredPolicy(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := acquire(ctx, root, Watch, limit(2))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := acquire(ctx, root, Watch, limit(2))
	if err != nil {
		t.Fatal(err)
	}
	if l, err := acquire(ctx, root, Watch, limit(2)); l != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("over capacity: %v %v", l, err)
	}
	if l, err := acquire(ctx, root, Watch, limit(1)); l != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("lowered policy: %v %v", l, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if l, err := acquire(ctx, root, Watch, limit(0)); l != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("off admitted", err)
	}
	next, err := acquire(ctx, root, Watch, limit(2))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if !Borrow(WithLease(ctx, next), root, Watch) || Borrow(WithLease(ctx, next), root, Notify) {
		t.Fatal("typed lease mismatch")
	}
}

func TestInheritedLeaseSurvivesParentClose(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	parent, err := acquire(ctx, root, Notify, limit(1))
	if err != nil {
		t.Fatal(err)
	}
	child, err := InheritFD(ctx, root, Notify, parent.Slot(), int(parent.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if l, err := acquire(ctx, root, Notify, limit(1)); l != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("parent close released inherited capacity", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquire(ctx, root, Notify, limit(1))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
}

func TestPolicyFailureCannotAdmitAndUnlockedFDIsNotLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if l, err := acquire(ctx, root, Watch, func(context.Context, policy.Spec) (int, error) { return 0, errors.New("unreadable") }); l != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatal("lookup failure admitted", err)
	}
	lease, err := acquire(ctx, root, Notify, limit(1))
	if err != nil {
		t.Fatal(err)
	}
	slot := lease.Slot()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(root + "/coord/n-00")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if inherited, err := InheritFD(ctx, root, Notify, slot, int(d.Fd())); inherited != nil || err == nil {
		t.Fatal("unlocked descriptor borrowed capacity")
	}
}

func TestUnprovenCandidateDoesNotCloseOriginalDescriptor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	holder, err := acquire(ctx, root, Notify, limit(1))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	sentinel, err := os.CreateTemp(t.TempDir(), "sentinel")
	if err != nil {
		t.Fatal(err)
	}
	defer sentinel.Close()
	if lease, err := InheritFD(ctx, root, Notify, holder.Slot(), int(sentinel.Fd())); lease != nil || !errors.Is(err, ErrUnavailable) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("sentinel admitted: %v", err)
	}
	if _, err := sentinel.WriteString("still owned by caller"); err != nil {
		t.Fatal("rejected candidate closed caller's original", err)
	}
	if _, err := holder.File().Stat(); err != nil {
		t.Fatal("rejected candidate closed real holder", err)
	}
}

func TestAbsentCandidateIsRejectedBeforeOpeningState(t *testing.T) {
	ctx := context.Background()
	sentinel, err := os.CreateTemp(t.TempDir(), "sentinel")
	if err != nil {
		t.Fatal(err)
	}
	defer sentinel.Close()
	// A positive candidate above the process limit stays absent even if other
	// runtime goroutines open descriptors while state discovery would run.
	root := t.TempDir() + "/not-created"
	const missing = 1<<31 - 1
	var descriptorLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &descriptorLimit); err != nil {
		t.Fatal(err)
	}
	if missing <= descriptorLimit.Cur {
		t.Fatal("invalid-FD fixture must exceed the process limit")
	}

	if lease, err := InheritFD(ctx, root, Notify, 0, missing); lease != nil || !errors.Is(err, ErrUnavailable) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("absent descriptor admitted: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opened state before duplicating absent descriptor: %v", err)
	}
	if _, err := sentinel.Stat(); err != nil {
		t.Fatal("unrelated descriptor closed", err)
	}
}
