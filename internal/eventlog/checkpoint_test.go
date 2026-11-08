package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistedCheckpointRetainsIdentityAndReplayOverlap(t *testing.T) {
	ctx := context.Background()
	log := Open(t.TempDir())
	if err := log.Append(ctx, &Event{Event: "before"}); err != nil {
		t.Fatal(err)
	}
	head, err := log.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := head.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var restored Checkpoint
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	cursor, err := restored.Cursor()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(ctx, &Event{Event: "during"}); err != nil {
		t.Fatal(err)
	}
	batch, err := log.Replay(ctx, cursor)
	if err != nil || len(batch.Events) != 1 || batch.Events[0].Event != "during" {
		t.Fatalf("replay=%v err=%v", batch, err)
	}
	for _, name := range []string{historyName, currentName, seqName} {
		if err := os.Remove(filepath.Join(log.Dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Append(ctx, &Event{Event: "replacement"}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Replay(ctx, cursor); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("old checkpoint admitted after recreation: %v", err)
	}
	restored.Epoch = ""
	if _, err := restored.Cursor(); err == nil {
		t.Fatal("identity-less checkpoint accepted")
	}
}
