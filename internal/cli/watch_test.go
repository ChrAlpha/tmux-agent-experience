package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func watchPulse(t *testing.T, ctx context.Context, log *eventlog.Log, count int) {
	t.Helper()
	for range count {
		if err := log.Append(ctx, &eventlog.Event{Event: "Pulse"}); err != nil {
			t.Fatal(err)
		}
	}
}

func watchCapture(ctx context.Context, log *eventlog.Log, v query.View) (query.Snapshot, error) {
	seq, err := log.Head(ctx)
	return query.Snapshot{Seq: seq, View: v}, err
}

// A follower that falls behind the retained history gets a new snapshot and
// goes on from it, never a suffix passed off as complete.
func TestWatchSendsASnapshotWhenEventsWereLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 500
	var frames []string
	var resumed int64
	stop := errors.New("followed after the second snapshot")
	s := watchStream{
		log:     log,
		capture: func(ctx context.Context) (query.Snapshot, error) { return watchCapture(ctx, log, query.View{}) },
		rescan:  func(context.Context) ([]query.Pane, error) { return nil, nil },
		emit: func(doc map[string]any) error {
			frames = append(frames, doc["type"].(string))
			switch {
			case len(frames) == 1:
				watchPulse(t, ctx, log, 30) // rotates several times before the next look
			case doc["type"] == "snapshot":
				resumed = doc["seq"].(int64)
				watchPulse(t, ctx, log, 1)
			case doc["type"] == "event" && resumed != 0:
				if e := doc["event"].(eventlog.Event); e.Seq != resumed+1 {
					t.Fatalf("after the snapshot at %d came %d", resumed, e.Seq)
				}
				return stop
			}
			return nil
		},
	}
	if err := s.run(ctx, -1); !errors.Is(err, stop) {
		t.Fatalf("watch = %v, frames %v", err, frames)
	}
	if frames[0] != "snapshot" || frames[1] != "snapshot" || frames[len(frames)-1] != "event" {
		t.Fatalf("frames %v", frames)
	}
}

// A resume from a cursor the log no longer continues starts with a
// snapshot instead.
func TestWatchResumeFromALostCursorStartsWithASnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 500
	watchPulse(t, ctx, log, 1)
	first, _ := log.Head(ctx)
	watchPulse(t, ctx, log, 30)
	stop := errors.New("snapshot observed")
	s := watchStream{
		log:     log,
		capture: func(ctx context.Context) (query.Snapshot, error) { return watchCapture(ctx, log, query.View{}) },
		rescan:  func(context.Context) ([]query.Pane, error) { return nil, nil },
		emit: func(doc map[string]any) error {
			if doc["type"] != "snapshot" {
				t.Fatalf("a lost cursor resumed with %v", doc)
			}
			return stop
		},
	}
	if err := s.run(ctx, first); !errors.Is(err, stop) {
		t.Fatalf("watch = %v", err)
	}
}

func TestWatchNumericResumeKeepsEventsThenPanes(t *testing.T) {
	ctx := context.Background()
	log := eventlog.Open(t.TempDir())
	first := eventlog.Event{Event: "First"}
	if err := log.Append(ctx, &first); err != nil {
		t.Fatal(err)
	}
	watchPulse(t, ctx, log, 1)
	var frames []string
	stop := errors.New("resumed panes observed")
	s := watchStream{
		log: log,
		capture: func(context.Context) (query.Snapshot, error) {
			t.Fatal("valid numeric resume was replaced with a snapshot")
			return query.Snapshot{}, nil
		},
		rescan: func(context.Context) ([]query.Pane, error) { return []query.Pane{}, nil },
		emit: func(doc map[string]any) error {
			frames = append(frames, doc["type"].(string))
			if doc["type"] == "panes" {
				return stop
			}
			if e := doc["event"].(eventlog.Event); e.Seq != first.Seq+1 {
				t.Fatalf("numeric replay = %+v", e)
			}
			return nil
		},
	}
	if err := s.run(ctx, first.Seq); !errors.Is(err, stop) || strings.Join(frames, ",") != "event,panes" {
		t.Fatalf("numeric resume = %v, %v", frames, err)
	}
}

// A watch of an empty log starts after event 0 and follows its first events,
// numbered from the time they are logged.
func TestWatchEmptySnapshotFollowsFirstTimestampEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	stop := errors.New("first event observed")
	s := watchStream{
		log:     log,
		capture: func(ctx context.Context) (query.Snapshot, error) { return watchCapture(ctx, log, query.View{}) },
		rescan:  func(context.Context) ([]query.Pane, error) { return nil, nil },
		emit: func(doc map[string]any) error {
			if doc["type"] == "snapshot" {
				if doc["seq"].(int64) != 0 {
					t.Fatalf("empty snapshot cursor = %v", doc)
				}
				watchPulse(t, ctx, log, 1)
				return nil
			}
			if e := doc["event"].(eventlog.Event); e.Seq < time.Now().Add(-time.Hour).UnixMicro() {
				t.Fatalf("first event sequence = %d", e.Seq)
			}
			return stop
		},
	}
	if err := s.run(ctx, -1); !errors.Is(err, stop) {
		t.Fatalf("empty startup = %v", err)
	}
}

// A wait after send delivered its prompt keeps saying so whatever ends it,
// and what to do next names the target as typed.
func TestSendWaitFailureKeepsTheDeliveryOutcome(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	store := session.Store{Dir: log.Dir}
	if err := store.Put(context.Background(), session.Record{Agent: "claude", ID: "session", Main: "idle", Turn: "t1", Sent: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := app{log: log, json: true, stdout: &stdout, stderr: &stderr}
	turn := "t1"
	code := a.awaitAndReportContext(context.Background(), "send", "%7", query.Session{Agent: "claude", ID: "session", State: "idle"}, 42,
		query.WaitOptions{States: []string{"idle"}, Prompted: &turn}, 300*time.Millisecond)
	var d struct {
		Error struct{ Code, Message string }
	}
	if code != ExitTimeout || json.Unmarshal(stdout.Bytes(), &d) != nil || d.Error.Code != "timeout" {
		t.Fatalf("a wait that timed out after delivery: %d %s", code, stdout.String())
	}
	if !strings.Contains(d.Error.Message, fmt.Sprintf("prompt delivered at seq %d", 42)) || !strings.Contains(d.Error.Message, "do not repeat") {
		t.Fatalf("the delivery was lost: %s", d.Error.Message)
	}
	if want := nextPrefix + "the prompt arrived and what became of it is unknown: run tmag wait %7 --for idle,blocked"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", stderr.String(), want)
	}
}

// Readers take no lock of the log: a writer holding it, or stuck with it,
// stalls no view, wait or diagnostic.
func TestReadCommandsDoNotWaitForWriters(t *testing.T) {
	// Inspect loads the world before reading history. Restrict that discovery
	// to a scratch tmux namespace, never the developer's server.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	for _, command := range []string{"list", "inspect", "wait", "state diagnostic"} {
		t.Run(command, func(t *testing.T) {
			log := eventlog.Open(t.TempDir())
			store := session.Store{Dir: log.Dir}
			if err := store.Put(context.Background(), session.Record{Agent: "claude", ID: "session", Main: "idle"}); err != nil {
				t.Fatal(err)
			}
			watchPulse(t, context.Background(), log, 1)
			held, release := make(chan struct{}), make(chan struct{})
			go func() { _ = log.WithLock(context.Background(), func() error { close(held); <-release; return nil }) }()
			<-held
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			a := app{ctx: ctx, log: log, stdout: &stdout, stderr: &stderr}
			if command == "state diagnostic" {
				if got := a.checkState(ctx); got.Status != "ok" {
					t.Fatalf("diagnostic waited for the writer: %+v", got)
				}
				return
			}
			args := []string{command}
			if command == "inspect" || command == "wait" {
				args = append(args, "session")
			}
			args = append(args, "--json")
			if code := a.run(args); code != ExitOK {
				t.Fatalf("%s: %d %s %s", command, code, stdout.String(), stderr.String())
			}
		})
	}
}
