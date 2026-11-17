package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// An end whose window to tell its askers closed still closes what others
// owed the session that ended, whenever its settling runs.
func TestALateSettlingOfAnEndStillClosesWhatWasOwedIt(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ctx := context.Background()
	dir := t.TempDir()
	log, store := eventlog.Open(dir), session.Store{Dir: dir}
	ended := eventlog.Actor{Agent: proc.Claude, Session: "a1"}
	b1 := session.Record{Agent: proc.Codex, ID: "b1", Main: adapter.Idle}
	b1.Owe(ended, eventlog.OweReply, 1, time.Now())
	if err := store.Put(ctx, b1); err != nil {
		t.Fatal(err)
	}
	end := eventlog.Event{Event: eventlog.End, Agent: ended.Agent, Session: ended.Session, Time: time.Now().Add(-2 * session.EndWindow)}
	if err := log.Append(ctx, &end); err != nil {
		t.Fatal(err)
	}
	a := &app{ctx: ctx, log: log, stdout: io.Discard, stderr: io.Discard}
	if err := a.settleEnd(end.Seq); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Get(ctx, b1.Agent, b1.ID); got.Owes != nil {
		t.Fatalf("b1 still owes %+v to a session that ended", got.Owes)
	}
}
