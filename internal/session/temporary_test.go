package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
)

func TestPublicationReusesFixedTemporaryAndPreservesAliases(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	rec := Record{Agent: "claude", ID: "one", Main: "idle", Since: t0}
	publish := func() error {
		return store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.PutContext(ctx, rec) })
	}
	if err := publish(); err != nil {
		t.Fatal(err)
	}
	tmp := store.temporary(rec.Agent, coord.Slot(sessionKey(rec.Agent, rec.ID)))
	if err := os.WriteFile(tmp, []byte(`{"unfinished":`), 0600); err != nil {
		t.Fatal(err)
	}
	rec.Main = "working"
	if err := publish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved temporary was not reused: %v", err)
	}
	if got, ok, err := store.Get(rec.Agent, rec.ID); err != nil || !ok || got.Main != rec.Main {
		t.Fatalf("publication = %+v %v %v", got, ok, err)
	}
	foreign := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, tmp); err != nil {
		t.Fatal(err)
	}
	if err := publish(); err == nil {
		t.Fatal("published through a temporary alias")
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "preserve" {
		t.Fatalf("alias target changed: %q %v", b, err)
	}
	if got, ok, err := store.Get(rec.Agent, rec.ID); err != nil || !ok || got.Main != rec.Main {
		t.Fatalf("refusal changed record: %+v %v %v", got, ok, err)
	}
}
