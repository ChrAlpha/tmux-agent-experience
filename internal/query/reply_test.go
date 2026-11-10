package query

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// Claude Code may write a turn's reply to its transcript after the Stop
// hook that ended the turn: a transcript that holds the turn's prompt and
// no reply yet is read again until the reply is there, and only a turn
// that still has none once the agent had time to write it ended without one.
func TestLastReplyWaitsForTheReplyToBeWritten(t *testing.T) {
	// Load discovers panes; keep it to a scratch tmux namespace, never the
	// developer's server.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	setup := func(t *testing.T) (session.Store, func(string)) {
		path := filepath.Join(t.TempDir(), "s.jsonl")
		write := func(entry string) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			if _, err := f.WriteString(entry + "\n"); err != nil {
				t.Error(err)
			}
		}
		write(`{"type":"user","promptId":"t1","message":{"role":"user","content":"hi"}}`)
		store := session.Store{Dir: t.TempDir()}
		rec := session.Record{Agent: "claude", ID: "session-1", Main: "idle", Turn: "t1", Transcript: path}
		if err := store.Lock(context.Background(), rec.Agent, rec.ID, func() error { return store.PutContext(context.Background(), rec) }); err != nil {
			t.Fatal(err)
		}
		return store, write
	}
	t.Run("written late", func(t *testing.T) {
		t.Parallel()
		store, write := setup(t)
		time.AfterFunc(500*time.Millisecond, func() {
			write(`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}]}}`)
		})
		if _, r, err := LastReply(context.Background(), store, "session-1"); err != nil || r.Turn != "t1" || r.Text != "hello" {
			t.Fatalf("a reply written after the turn ended: %+v %v", r, err)
		}
	})
	t.Run("never written", func(t *testing.T) {
		t.Parallel()
		store, _ := setup(t)
		started := time.Now()
		_, _, err := LastReply(context.Background(), store, "session-1")
		if !errors.Is(err, ErrNoReply) || !strings.Contains(err.Error(), "ended without one") {
			t.Fatalf("a turn without a reply: %v", err)
		}
		if waited := time.Since(started); waited < transcriptLag {
			t.Fatalf("a turn without a reply yet was taken as ended after %s", waited)
		}
	})
}
