package contextio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
)

func TestContextFilteringPreservesOtherErrorBranches(t *testing.T) {
	storage := &os.PathError{Op: "write", Path: "metadata", Err: io.ErrUnexpectedEOF}
	for _, err := range []error{storage, errors.Join(storage, context.DeadlineExceeded), fmt.Errorf("operation: %w", errors.Join(context.Canceled, storage, fmt.Errorf("budget: %w", context.DeadlineExceeded)))} {
		got := WithoutContextError(WithoutContextError(err, context.DeadlineExceeded), context.Canceled)
		var path *os.PathError
		if !errors.Is(got, io.ErrUnexpectedEOF) || !errors.As(got, &path) || path != storage || errors.Is(got, context.DeadlineExceeded) || errors.Is(got, context.Canceled) {
			t.Fatalf("real branch lost or deadline retained: %v", got)
		}
	}
	for _, err := range []error{context.Canceled, fmt.Errorf("metadata: %w", context.DeadlineExceeded), errors.Join(context.Canceled, context.DeadlineExceeded)} {
		if got := WithoutContextError(WithoutContextError(err, context.DeadlineExceeded), context.Canceled); got != nil {
			t.Fatalf("pure context failure retained: %v", got)
		}
	}
}

func TestDeadlineFilteringDoesNotRemoveAnotherCancellation(t *testing.T) {
	got := WithoutContextError(errors.Join(context.DeadlineExceeded, context.Canceled), context.DeadlineExceeded)
	if !errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("lost independent cancellation: %v", got)
	}
}

type budgetLikeFault struct{}

func (*budgetLikeFault) Error() string { return "storage fault with deadline matching" }
func (*budgetLikeFault) Is(target error) bool {
	return target == context.DeadlineExceeded //nolint:errorlint // This synthetic Is method matches only the canonical sentinel.
}
func TestCustomDeadlineMatchIsNotCanonicalBudgetEvidence(t *testing.T) {
	fault := &budgetLikeFault{}
	if got := WithoutContextError(errors.Join(fault, context.DeadlineExceeded), context.DeadlineExceeded); !errors.Is(got, fault) {
		t.Fatalf("custom deadline match hid a genuine fault: %v", got)
	}
}
