package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

func TestContactOnlySavePublishesCompleteEventWithoutChangingSince(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := Store{Dir: root}
	log := eventlog.Open(root)
	since := time.Unix(10, 123).UTC()
	rec := Record{Agent: "claude", ID: "thread", Main: "working", Turn: "turn", Since: since}
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error {
		var err error
		rec, err = store.Save(ctx, log, rec, "UserPromptSubmit")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec.LastHookAt = time.Unix(20, 456).UTC()
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error {
		var err error
		rec, err = store.Save(ctx, log, rec, "PreToolUse")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	events, _, err := log.Since(ctx, 0)
	if err != nil || len(events) != 2 || events[1].State != "working" || events[1].Turn != "turn" || events[1].Since == nil || !events[1].Since.Equal(since) || !events[1].LastHookAt.Equal(rec.LastHookAt) {
		t.Fatalf("incomplete contact publication: %+v %v", events, err)
	}
	current, ok, err := store.Get(rec.Agent, rec.ID)
	if err != nil || !ok || current.Unlogged() || !current.Since.Equal(since) {
		t.Fatalf("contact changed state age or stayed pending: %+v %v", current, err)
	}
}
func TestCompleteReadsRefuseFirstFaultAndPreserveOriginal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := Store{Dir: root}
	rec := Record{Agent: "codex", ID: "thread", Main: "idle"}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "codex", "thread.json")
	bad := []byte("private prompt/decoder sentinel")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() error{
		func() error { _, _, err := store.Get("codex", "thread"); return err },
		func() error {
			records, err := store.AllContext(ctx)
			if len(records) != 0 {
				t.Fatal("published partial catalog")
			}
			return err
		},
	} {
		err := read()
		var fault *RecordError
		if !errors.As(err, &fault) || fault.Agent != "codex" || fault.ID != "thread" || fault.Category != "decode" || strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("fault became absence/raw decoder output: %v", err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(bad) {
		t.Fatal("reader changed original", err)
	}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	if records, err := store.All(); err != nil || len(records) != 1 || !records[0].LastHookAt.IsZero() {
		t.Fatalf("repaired legacy record unreadable: %+v %v", records, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.GetContext(canceled, "codex", "thread"); !errors.Is(err, context.Canceled) {
		t.Fatal("lost parent cancellation", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((4 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, _, err := store.GetContext(ctx, "codex", "thread"); err == nil {
		t.Fatal("oversized metadata treated as absent/readable")
	}
}

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
		t.Fatalf("unsafe record became a complete view: %v %v", got, err)
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

func TestCompleteReadsSurviveConcurrentRepublication(t *testing.T) {
	ctx := context.Background()
	store := Store{Dir: t.TempDir()}
	const records = 8
	put := func(i, n int) error {
		rec := Record{Agent: proc.Codex, ID: fmt.Sprintf("thread-%d", i), Main: "idle", LastHookAt: time.Unix(int64(n), 0).UTC()}
		return store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.PutContext(ctx, rec) })
	}
	for i := range records {
		if err := put(i, 0); err != nil {
			t.Fatal(err)
		}
	}
	stop, published := make(chan struct{}), make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				published <- n
				return
			default:
			}
			if err := put(n%records, n+1); err != nil {
				t.Error(err)
				published <- n
				return
			}
			n++
		}
	}()
	defer func() {
		close(stop)
		if n := <-published; n == 0 {
			t.Error("no publication overlapped the reads")
		}
	}()
	for range 300 {
		if all, err := store.AllContext(ctx); err != nil || len(all) != records {
			t.Fatalf("complete view during republication: %d records, %v", len(all), err)
		}
		if _, ok, err := store.GetContext(ctx, proc.Codex, "thread-3"); err != nil || !ok {
			t.Fatalf("record during republication: %v %v", ok, err)
		}
	}
}
