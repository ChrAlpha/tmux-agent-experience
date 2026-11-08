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
	if got, err := s.AllContext(context.Background()); err != nil || len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("view=%v err=%v", got, err)
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
