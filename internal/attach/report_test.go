package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPaneRepairReportsKeepFaultsWithoutPersistingErrorText(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deferred := ReconcileReport{Complete: true}
	deferred.add(ctx, "pane", context.Canceled, "codex", "thread")
	if deferred.Complete || deferred.Deferred != 1 || deferred.FaultCount != 0 || deferred.Err != nil {
		t.Fatalf("pure expiration became a fault: %+v", deferred)
	}
	fault := errors.New("private I/O detail sentinel")
	failed := ReconcileReport{Complete: true}
	failed.add(ctx, "pane", errors.Join(fault, context.Canceled), "codex", "thread")
	if failed.Complete || failed.FaultCount != 1 || !errors.Is(failed.Err, fault) || errors.Is(failed.Err, context.Canceled) {
		t.Fatalf("joined fault lost or misclassified: %+v", failed)
	}
	encoded, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(fault.Error())) || bytes.Contains(encoded, []byte(`"Err"`)) {
		t.Fatal("in-memory error tree escaped to persisted metadata")
	}
	// An independent timeout from an operation is a fault while its enclosing
	// context is healthy; a later repair deadline must not erase that evidence.
	earlier := ReconcileReport{Complete: true}
	earlier.add(context.Background(), "pane", context.DeadlineExceeded, "codex", "thread")
	if earlier.FaultCount != 1 || earlier.Deferred != 0 || !errors.Is(earlier.Err, context.DeadlineExceeded) {
		t.Fatalf("independent operation timeout hidden: %+v", earlier)
	}
}
