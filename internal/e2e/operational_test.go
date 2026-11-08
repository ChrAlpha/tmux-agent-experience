package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestCorruptRecordPreservesPaneMarkersAndRecoversFollower(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	ctx := context.Background()
	store := session.Store{Dir: h.state}
	rec, found, err := store.Get("claude", sidA)
	if err != nil || !found {
		t.Fatal("missing healthy fixture", err)
	}
	path := filepath.Join(h.state, "sessions", "claude", sidA+".json")
	if err := store.Lock(ctx, "claude", sidA, func() error { return os.WriteFile(path, []byte("{"), 0o600) }); err != nil {
		t.Fatal(err)
	}
	// Simulate a writer that already published attachment evidence but did not
	// append its event. Diagnostic suppression must never acknowledge this mark.
	h.tmux("set-option", "-p", "-t", pane, "@tmag_change", eventlog.Attach)
	result := h.tmag("", "prune")
	if result.code != 1 || !strings.Contains(result.out, "records_unavailable") {
		t.Fatalf("corrupt prune: %+v", result)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var corrupted tmux.Pane
	for _, p := range w.Panes {
		if p.ID == pane {
			corrupted = p
		}
	}
	if corrupted.Options["unavailable"] != "on" || corrupted.Options["change"] != eventlog.Attach || corrupted.Options["session"] != sidA || corrupted.Options["state"] != "idle" {
		t.Fatalf("lost repair evidence or missing overlay: %+v", corrupted.Options)
	}
	if got := h.tmag("", "send", h.target(pane), "--json", "do not deliver"); got.code != 1 || !strings.Contains(got.out, "records_unavailable") {
		t.Fatalf("corrupt control accepted: %+v", got)
	}
	if got := h.tmag("", "watch"); got.code != 1 || !strings.Contains(got.out, "records_unavailable") {
		t.Fatalf("corrupt watch fabricated an image: %+v", got)
	}
	if err := store.Lock(ctx, "claude", sidA, func() error { return store.Put(rec) }); err != nil {
		t.Fatal(err)
	}
	if got := h.tmag("", "prune"); got.code != 0 {
		t.Fatalf("recovery: %+v", got)
	}
	w, err = attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range w.Panes {
		if p.ID == pane && (p.Options["unavailable"] != "" || p.Options["change"] != "" || p.Options["state"] != "idle") {
			t.Fatalf("projection did not recover: %+v", p.Options)
		}
	}
	f := h.follow()
	f.agrees(h)
}

func TestFreshListWithReconciliationOffNeedsNoSharedImage(t *testing.T) {
	h := start(t)
	h.tmux("set-option", "-g", "@tmag-reconcile-interval", "off")
	store := session.Store{Dir: h.state}
	if err := store.Put(session.Record{Agent: "claude", ID: sidA, Main: "idle"}); err != nil {
		t.Fatal(err)
	}
	r := h.tmag("", "list", "--json")
	var doc struct {
		Schema   string          `json:"schema"`
		Sessions []query.Session `json:"sessions"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.out), &doc) != nil || doc.Schema != "tmag/v1" || len(doc.Sessions) != 1 || doc.Sessions[0].ID != sidA || doc.Sessions[0].State != "idle" {
		t.Fatalf("valid fresh read depended on disabled cache: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(h.state, "observe", "image.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled trigger created an image", err)
	}
	if err := os.WriteFile(filepath.Join(h.state, "sessions", "claude", sidA+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.tmag("", "list", "--json"); r.code != 1 || !strings.Contains(r.out, `"code":"records_unavailable"`) {
		t.Fatalf("disabled optional repair hid a real record fault: %+v", r)
	}
}
