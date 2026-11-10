package attach

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// A record tmag cannot read is left as it is: reconciliation neither ends
// its session nor logs anything for it, and carries on with the others.
func TestReconcileLeavesUnreadableRecordsAlone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := session.Store{Dir: root}
	log := eventlog.Open(root)
	gone := session.Record{Agent: proc.Claude, ID: "gone", Main: "idle", Host: session.Host{PID: 99, Start: "s99"}}
	if err := store.Put(ctx, gone); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "claude", "broken.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(ctx, Env{Log: log, Store: store}, &World{Proc: proc.NewTable(nil)}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "{" {
		t.Fatal("the unreadable record changed", err)
	}
	if _, ok, _ := store.Get(ctx, proc.Claude, "gone"); ok {
		t.Fatal("a session whose host exited was not ended")
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil || len(events) != 1 || events[0].Event != eventlog.Exit || events[0].Session != "gone" {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
