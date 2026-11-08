package diagnostic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

func TestSummaryIsBoundedByCategoriesAndDoesNotPersistRawErrors(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for i := range 96 {
		f := Fault{Component: "reconcile", Operation: "session", Category: "decode", Agent: "codex", ID: fmt.Sprintf("session-%d", i)}
		if err := Record(ctx, root, f); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Read(ctx, root)
	if err != nil || len(s.Buckets) != 1 || s.Buckets[0].Count != 96 {
		t.Fatalf("summary=%+v err=%v", s, err)
	}
	if err := Record(ctx, root, Fault{Component: "hook", Operation: "run", Category: "secret-sentinel"}); err == nil {
		t.Fatal("stored raw category")
	}
	data, err := os.ReadFile(filepath.Join(root, "diagnostics", "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxBytes || bytes.Contains(data, []byte("secret-sentinel")) {
		t.Fatal("unbounded or raw diagnostic")
	}
}

func TestHookLogCeilingAndUnsafeObject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// Simulate a legacy record at the ceiling: the next serialized append rotates.
	if err := os.WriteFile(filepath.Join(root, "hook.log"), bytes.Repeat([]byte("x"), MaxLogBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	f := Fault{Component: "hook", Operation: "run", Category: "failed"}
	if err := AppendLog(ctx, root, f); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "hook.log"))
	if err != nil || st.Size() > MaxLogBytes {
		t.Fatalf("st=%v err=%v", st, err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "hook.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(root, "hook.log")); err != nil {
		t.Fatal(err)
	}
	if err := AppendLog(ctx, root, f); err == nil {
		t.Fatal("followed replaced hook log")
	}
	data, _ := os.ReadFile(other)
	if string(data) != "keep" {
		t.Fatal("modified symlink target")
	}
}

func TestClockRollbackKeepsSummaryReadable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fault := Fault{Component: "hook", Operation: "run", Category: "failed"}
	if err := Record(ctx, root, fault); err != nil {
		t.Fatal(err)
	}
	summary, err := Read(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	summary.Buckets[0].First = future
	summary.Buckets[0].Last = future.Add(time.Hour)
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteSlots(ctx, root, "diagnostics/summary.json", "diagnostics/.summary-00", ".summary-", 1, data); err != nil {
		t.Fatal(err)
	}
	if err := Record(ctx, root, fault); err != nil {
		t.Fatal(err)
	}
	got, err := Read(ctx, root)
	if err != nil || len(got.Buckets) != 1 {
		t.Fatalf("rollback poisoned summary: %+v %v", got, err)
	}
	bucket := got.Buckets[0]
	if bucket.Count != 2 || bucket.ClockRollbacks != 1 || !bucket.First.Before(future) || !bucket.Last.Equal(future.Add(time.Hour)) {
		t.Fatalf("untruthful clock range: %+v", bucket)
	}
	if err := Record(ctx, root, fault); err != nil {
		t.Fatal("could not record again after rollback", err)
	}
}
