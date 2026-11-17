package attach

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// Claude Code's status file says the turn is over while no hook did, as an
// interrupt leaves it: reconciliation applies the same idle transition and
// logs it as its own event.
func TestReconcileEndsATurnFromClaudesStatusFile(t *testing.T) {
	tb, err := proc.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	self, ok := tb.Get(os.Getpid())
	if !ok || self.Start == "" {
		t.Fatal("this process is not in its own process table")
	}
	w := &World{Proc: proc.NewTable([]proc.Process{{PID: self.PID, Start: self.Start, Comm: "claude"}})}
	for _, c := range []struct {
		name    string
		status  string
		updated time.Time
		want    string
		events  int
	}{
		{"fresh idle", "idle", time.Now().Add(time.Second), adapter.Idle, 1},
		{"stale idle", "idle", time.Now().Add(-time.Minute), adapter.Working, 0},
		{"busy", "busy", time.Now().Add(time.Second), adapter.Working, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			log, store := eventlog.Open(dir), session.Store{Dir: dir}
			statusFile := filepath.Join(t.TempDir(), "status.json")
			body, _ := json.Marshal(map[string]any{
				"sessionId": "c1", "status": c.status, "statusUpdatedAt": c.updated.UnixMilli(), "procStart": self.Start,
			})
			if err := os.WriteFile(statusFile, body, 0o600); err != nil {
				t.Fatal(err)
			}
			rec := session.Record{
				Agent: proc.Claude, ID: "c1", Main: adapter.Working, Turn: "t1",
				Host: session.Host{PID: self.PID, Start: self.Start, Role: proc.Terminal},
			}
			// The record's last hook contact is recent; only a later status
			// proves the turn over.
			rec.LastHookAt = time.Now()
			rec.StatusFile = statusFile
			if err := store.Put(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			if err := Reconcile(context.Background(), Env{Log: log, Store: store}, w); err != nil {
				t.Fatal(err)
			}
			got, ok, err := store.Get(context.Background(), proc.Claude, "c1")
			if err != nil || !ok {
				t.Fatal(err)
			}
			if got.State() != c.want {
				t.Fatalf("state = %s (%s), want %s", got.State(), got.Kind(), c.want)
			}
			events, _, err := log.Since(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != c.events || (c.events == 1 && events[0].Event != eventlog.Idle) {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}
