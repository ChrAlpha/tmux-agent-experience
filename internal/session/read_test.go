package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

func TestContextReadsRejectCanceledViewAndBlockingRecord(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Put(Record{Agent: proc.Claude, ID: "live"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "sessions", proc.Claude, "bad.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetContext(context.Background(), proc.Claude, "bad"); err == nil {
		t.Fatal("FIFO accepted")
	}
	if got, err := s.AllContext(context.Background()); err == nil || got != nil {
		t.Fatalf("view=%v err=%v", got, err)
	}
	catalog, err := s.ScanContext(context.Background())
	if err != nil || !catalog.Complete || catalog.IssueCount != 1 || len(catalog.Records) != 1 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.AllContext(ctx); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("partial canceled view=%v err=%v", got, err)
	}
	if _, ok, err := s.GetContext(ctx, proc.Claude, "live"); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRejectsCorruptAndMismatchedIdentity(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Put(Record{Agent: "codex", ID: "healthy"}); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"broken": "{", "alias": `{"agent":"codex","id":"healthy","host":{"pid":0,"role":""},"since":"0001-01-01T00:00:00Z","logged":{}}`} {
		if err := os.WriteFile(filepath.Join(s.Dir, "sessions", "codex", name+".json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.ScanContext(context.Background())
	if err != nil || c.IssueCount != 2 || len(c.Records) != 1 {
		t.Fatalf("catalog=%+v err=%v", c, err)
	}
	var unavailable *RecordsUnavailable
	if records, err := s.All(); records != nil || !errors.As(err, &unavailable) {
		t.Fatalf("records=%v err=%v", records, err)
	}
}
