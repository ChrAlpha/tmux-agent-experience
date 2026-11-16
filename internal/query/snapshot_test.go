package query

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// A status file left by a Claude host that exited proves nothing: the
// snapshot leaves the dead session out, and no Idle event after it recreates
// the session for a follower.
func TestDeadHostDoesNotGetNativeIdle(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	status := filepath.Join(t.TempDir(), "status.json")
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"sessionId": "dead-review", "procStart": "dead-start", "status": "idle", "statusUpdatedAt": now.UnixMilli(),
	})
	if err := os.WriteFile(status, body, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := session.Record{
		Agent: "claude", ID: "dead-review", Main: adapter.Working,
		Host:       session.Host{PID: 2147483647, Start: "dead-start"},
		LastHookAt: now.Add(-time.Second), StatusFile: status,
	}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	snap, err := TakeSnapshot(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("snapshot holds a dead host: %+v", snap.Sessions)
	}
	events, _, err := log.Since(ctx, snap.Seq)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Session == rec.ID {
			t.Fatalf("a dead host got an event after the snapshot: %+v", ev)
		}
	}
}
