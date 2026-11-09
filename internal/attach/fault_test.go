package attach

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestReconcileFaultPreservesUncertainResourcesAndPendingEvents(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := session.Store{Dir: root}
	log := eventlog.Open(root)
	rec := session.Record{Agent: "claude", ID: "pending", Main: "idle", Change: "Stop"}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "claude", "broken.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	world := &World{Proc: proc.NewTable(nil), Panes: []tmux.Pane{{ID: "%1", Server: "private", Options: map[string]string{"change": "Attach", "session": "pending", "agent": "claude"}}}}
	err := Reconcile(ctx, Env{Log: log, Store: store}, world)
	var fault *session.RecordError
	if !errors.As(err, &fault) || fault.ID != "broken" {
		t.Fatalf("repair hid metadata fault: %v", err)
	}
	kept, ok, err := store.Get("claude", "pending")
	if err != nil || !ok || kept.Change != "Stop" {
		t.Fatalf("unknown catalog consumed pending marker: %+v %v", kept, err)
	}
	if head, err := log.Head(ctx); err != nil || head.Seq != 0 {
		t.Fatalf("incomplete catalog caused a fabricated event: %+v %v", head, err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "{" {
		t.Fatal("damaged original changed", err)
	}
}

// A record can become unreadable after the catalog scan and before the pane
// lock. Keep its pending attachment instead of logging an incomplete event.
func TestAttachmentSettlementPreservesUnreadableSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := session.Store{Dir: root}
	log := eventlog.Open(root)
	if err := store.Put(session.Record{Agent: "claude", ID: "pending", Main: "idle"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "claude", "pending.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := Env{Log: log, Store: store}
	err := env.finish(ctx, tmux.Server{Socket: "unused"}, tmux.Pane{ID: "%1"}, Attachment{Agent: "claude", Session: "pending", Change: eventlog.Attach})
	var fault *session.RecordError
	if !errors.As(err, &fault) || fault.ID != "pending" {
		t.Fatalf("incomplete attachment event accepted: %v", err)
	}
	if head, err := log.Head(ctx); err != nil || head.Seq != 0 {
		t.Fatalf("fault logged an incomplete event: %+v %v", head, err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "{" {
		t.Fatal("damaged original changed", err)
	}
}
