package query

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestReplayedViewIncludesCompleteHookReportsAndEnds(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	start, err := log.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Unix(10, 23).UTC()
	snap := Snapshot{Seq: start.Seq, View: View{Sessions: []Session{{Agent: "claude", ID: "live", State: "idle", Since: &since, Turn: "old", Cwd: "old", Requests: []eventlog.Request{{ID: "old"}}}, {Agent: "claude", ID: "ended", State: "idle"}}, Panes: []Pane{{Server: "private", ID: "%1", Agent: "claude", Session: "live", Evidence: "process", View: "main"}, {Server: "private", ID: "%2", Agent: "claude", Session: "ended", Evidence: "process", View: "main"}}}}
	later := time.Unix(20, 45).UTC()
	observation := &session.Observation{HookAt: later, Realm: "realm", Host: "verified"}
	for _, e := range []eventlog.Event{
		{Agent: "claude", Session: "live", Event: eventlog.Send, State: "idle", Since: &since, Cwd: "new"},
		{Agent: "claude", Session: "live", Event: "PermissionRequest", State: "blocked", Kind: "permission", Turn: "turn", Since: &later, Cwd: "new", Requests: []eventlog.Request{{ID: "current", Kind: "permission"}}},
		{Agent: "claude", Session: "live", Event: "PostToolUse", State: "blocked", Kind: "permission", Turn: "turn", Since: &later, Observation: observation, Cwd: "new", Requests: []eventlog.Request{{ID: "current", Kind: "permission"}}},
		{Agent: "claude", Session: "ended", Event: eventlog.Exit},
	} {
		if err := log.Append(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	snap.Backlog, err = log.Replay(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	v, err := snap.ReplayedView(ctx)
	if err != nil || len(v.Sessions) != 1 {
		t.Fatalf("replay: %+v %v", v, err)
	}
	got := v.Sessions[0]
	if got.State != "blocked" || got.Kind != "permission" || got.Turn != "turn" || got.Observation == nil || !got.Observation.HookAt.Equal(later) || got.Since == nil || !got.Since.Equal(later) || got.Cwd != "new" || len(got.Requests) != 1 || got.Requests[0].ID != "current" || len(got.Panes) != 1 || v.Panes[1].Session != "" {
		t.Fatalf("complete reports/end not applied: %+v %+v", got, v.Panes)
	}
	if snap.Seq != start.Seq || len(snap.Sessions) != 2 || snap.Sessions[0].State != "idle" || snap.Panes[1].Session != "ended" {
		t.Fatal("display rewrote original image/checkpoint")
	}
	if b, err := log.Replay(ctx, snap.Backlog.Cursor); err != nil || len(b.Events) != 0 {
		t.Fatalf("replay handoff changed: %+v %v", b, err)
	}
}
func TestReplayedSendDoesNotInventConfirmationOrCallerProof(t *testing.T) {
	since := time.Unix(10, 0).UTC()
	old := &session.Observation{HookAt: since, Host: "unknown"}
	snap := Snapshot{View: View{Sessions: []Session{{Agent: "codex", ID: "thread", State: "idle", Since: &since, Observation: old}}, Panes: []Pane{}}, Backlog: eventlog.Batch{Events: []eventlog.Event{{Agent: "codex", Session: "thread", Event: eventlog.Send, State: "idle", Since: &since, Observation: old}}}}
	v, err := snap.ReplayedView(context.Background())
	if err != nil || len(v.Sessions) != 1 || v.Sessions[0].State != "idle" || !v.Sessions[0].Since.Equal(since) || v.Sessions[0].Observation != old || len(v.records) != 0 || len(v.links) != 0 {
		t.Fatalf("unconfirmed send became state/caller proof: %+v %v", v, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snap.ReplayedView(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("replay ignored parent cancellation", err)
	}
}

func TestReplayAttachmentDoesNotInferClientView(t *testing.T) {
	ref := &eventlog.PaneRef{Server: "private", ID: "%1"}
	snap := Snapshot{View: View{Sessions: []Session{{Agent: "codex", ID: "thread", State: "idle"}}, Panes: []Pane{{Server: ref.Server, ID: ref.ID, Agent: "codex", Session: "thread", Evidence: "title-id", View: "subagent"}}}, Backlog: eventlog.Batch{Events: []eventlog.Event{{Agent: "codex", Session: "thread", Event: eventlog.Attach, State: "idle", Evidence: "title-id", Pane: ref}}}}
	// A state-only report leaves the captured attachment/view untouched.
	snap.Backlog.Events[0].Event = "Observation"
	v, err := snap.ReplayedView(context.Background())
	if err != nil || v.Panes[0].View != "subagent" {
		t.Fatalf("state report lost captured view: %+v %v", v, err)
	}
	snap.Backlog.Events[0].Event = eventlog.Attach
	v, err = snap.ReplayedView(context.Background())
	if err != nil || v.Panes[0].View != "" {
		t.Fatalf("Attach invented a view despite omitting process/title identity: %+v %v", v, err)
	}
	snap.Backlog.Events = append(snap.Backlog.Events, eventlog.Event{Agent: "codex", Session: "other", State: "idle", Event: eventlog.Attach, Pane: ref, Evidence: "title-id"})
	v, err = snap.ReplayedView(context.Background())
	if err != nil || v.Panes[0].Session != "other" || v.Panes[0].View != "" || len(v.Sessions[0].Panes) != 0 || len(v.Sessions[1].Panes) != 1 {
		t.Fatalf("changed attachment invented view or retained wrong binding: %+v %v", v, err)
	}
	snap.Backlog.Events = append(snap.Backlog.Events, eventlog.Event{Agent: "codex", Session: "other", Event: eventlog.End})
	v, err = snap.ReplayedView(context.Background())
	if err != nil || len(v.Sessions) != 1 || v.Panes[0].Session != "" {
		t.Fatalf("end left a replayed attachment: %+v %v", v, err)
	}
}

// Cancel while processing an ordinary large metadata image, without a timing
// race or a sleep. Err remains safe if called concurrently like any Context.
type replayCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int64
	at     int64
}

func (c *replayCancelContext) Err() error {
	if c.checks.Add(1) == c.at {
		c.cancel()
	}
	return c.Context.Err()
}
func TestReplayLargeImageHonorsCancellationThroughoutWork(t *testing.T) {
	snap := Snapshot{}
	for i := range 10000 {
		id := fmt.Sprintf("thread-%05d", i)
		pane := fmt.Sprintf("%%%d", i)
		snap.Sessions = append(snap.Sessions, Session{Agent: "codex", ID: id, State: "idle"})
		snap.Panes = append(snap.Panes, Pane{Server: "private", ID: pane, Agent: "codex", Session: id, Evidence: "title-id", View: "subagent"})
		snap.Backlog.Events = append(snap.Backlog.Events, eventlog.Event{Agent: "codex", Session: id, Event: "Observation", State: "idle"})
	}
	for _, at := range []int64{256, 22000, 42000} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &replayCancelContext{Context: parent, cancel: cancel, at: at}
			v, err := snap.ReplayedView(ctx)
			if !errors.Is(err, context.Canceled) || len(v.Sessions) != 0 || len(v.Panes) != 0 || ctx.checks.Load() > at+1 {
				t.Fatalf("large replay continued or published partial data after cancellation: checks=%d err=%v", ctx.checks.Load(), err)
			}
		})
	}
}
