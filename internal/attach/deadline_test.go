package attach

import (
	"context"
	"errors"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestCanceledDiscoveryCannotReconcileAbsence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w, err := Load(ctx); w != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("partial world=%v err=%v", w, err)
	}
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	rec := session.Record{Agent: proc.Claude, ID: "preserve", Host: session.Host{PID: 123, Start: "known"}, Main: "idle"}
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(ctx, Env{Log: log, Store: store}, &World{Proc: proc.NewTable(nil)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reconciliation lost context error: %v", err)
	}
	if _, ok, err := store.Get(context.Background(), rec.Agent, rec.ID); !ok || err != nil {
		t.Fatalf("canceled reconciliation removed session: ok=%v err=%v", ok, err)
	}
}

func TestUncertainProcessCannotEndSession(t *testing.T) {
	rec := session.Record{Agent: proc.Claude, ID: "preserve", Host: session.Host{PID: 123, Start: "known"}}
	w := &World{Proc: proc.NewTable([]proc.Process{{PID: 123, Uncertain: true}})}
	live, gone := Live(w, []session.Record{rec})
	if len(live) != 1 || len(gone) != 0 {
		t.Fatalf("incomplete process evidence ended session: live=%v gone=%v", live, gone)
	}
}

func TestIncompleteProcessTableCannotAuthorizeAnyExit(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	rec := session.Record{Agent: proc.Claude, ID: "preserve", Host: session.Host{PID: 456, Start: "known"}, Main: "idle"}
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	w := &World{Proc: proc.NewTable([]proc.Process{{PID: 123, Uncertain: true}})}
	if err := Reconcile(context.Background(), Env{Log: log, Store: store}, w); err == nil {
		t.Fatal("incomplete process evidence accepted for reconciliation")
	}
	if _, ok, err := store.Get(context.Background(), rec.Agent, rec.ID); !ok || err != nil {
		t.Fatalf("incomplete discovery authorized an exit: ok=%v err=%v", ok, err)
	}
}
