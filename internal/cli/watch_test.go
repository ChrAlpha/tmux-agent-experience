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

func watchCapture(t *testing.T, ctx context.Context, log *eventlog.Log, v query.View, during func()) (query.Snapshot, error) {
	t.Helper()
	start, err := log.Head(ctx)
	if err != nil {
		return query.Snapshot{}, err
	}
	if during != nil {
		during()
	}
	b, err := log.Replay(ctx, start)
	return query.Snapshot{Seq: start.Seq, View: v, Backlog: b}, err
}

func TestWatchPublishesCopiedBacklogThenTerminatesAtLostHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 1
	watchPulse(t, ctx, log, 1)
	var frames []string
	s := watchStream{
		log: log,
		capture: func(ctx context.Context) (query.Snapshot, error) {
			return watchCapture(t, ctx, log, query.View{}, func() { watchPulse(t, ctx, log, 1) })
		},
		rescan: func(context.Context) ([]query.Pane, error) { return nil, nil },
		emit: func(doc map[string]any) error {
			frames = append(frames, doc["type"].(string))
			if doc["type"] == "snapshot" {
				// A paused output callback must not hold the log lock. Its
				// copied backlog remains deliverable after these rotations.
				watchPulse(t, ctx, log, 5)
			}
			return nil
		},
	}
	err := s.run(ctx, -1, false)
	if !errors.Is(err, eventlog.ErrHistoryUnavailable) || strings.Join(frames, ",") != "snapshot,event" {
		t.Fatalf("publication/handoff = %v, %v", frames, err)
	}
	var stdout, stderr bytes.Buffer
	a := app{json: true, stdout: &stdout, stderr: &stderr}
	if code := a.watchEnd(err); code != ExitError || !strings.Contains(stdout.String(), `"code":"history_unavailable"`) {
		t.Fatalf("terminal v1 envelope = %d %s", code, stdout.String())
	}
}

func TestWatchResyncReplacesSessionsAndPanesInOneDocument(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 1
	old := query.View{Sessions: []query.Session{{ID: "old", Agent: "claude", State: "idle"}}, Panes: []query.Pane{{ID: "%1", Session: "old"}}}
	fresh := query.View{Sessions: []query.Session{}, Panes: []query.Pane{}}
	captures := 0
	stop := errors.New("replacement observed")
	s := watchStream{
		log: log,
		capture: func(ctx context.Context) (query.Snapshot, error) {
			captures++
			v := old
			if captures > 1 {
				v = fresh
			}
			return watchCapture(t, ctx, log, v, nil)
		},
		rescan: func(context.Context) ([]query.Pane, error) {
			t.Fatal("a stale pane writer ran across replacement")
			return nil, nil
		},
		emit: func(doc map[string]any) error {
			if doc["type"] != "snapshot" {
				t.Fatalf("partial replacement document: %v", doc)
			}
			if captures == 1 {
				watchPulse(t, ctx, log, 3)
				return nil
			}
			if len(doc["sessions"].([]query.Session)) != 0 || len(doc["panes"].([]query.Pane)) != 0 {
				t.Fatalf("replacement retained disappeared objects: %v", doc)
			}
			return stop
		},
	}
	if err := s.run(ctx, -1, true); !errors.Is(err, stop) || captures != 2 {
		t.Fatalf("resync = %d captures, %v", captures, err)
	}
}

func TestWatchResyncBoundsRepeatedLostHandoffs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 1
	captures := 0
	s := watchStream{
		log: log,
		capture: func(ctx context.Context) (query.Snapshot, error) {
			captures++
			return watchCapture(t, ctx, log, query.View{}, nil)
		},
		rescan: func(context.Context) ([]query.Pane, error) { return nil, nil },
		emit: func(doc map[string]any) error {
			if doc["type"] == "snapshot" {
				watchPulse(t, ctx, log, 3)
			}
			return nil
		},
	}
	err := s.run(ctx, -1, true)
	if !errors.Is(err, eventlog.ErrHistoryUnavailable) || !strings.Contains(err.Error(), "resync limit") || captures != 1+watchResetLimit {
		t.Fatalf("churn bound = %d captures, %v", captures, err)
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
	if err := s.run(ctx, first.Seq, false); !errors.Is(err, stop) || strings.Join(frames, ",") != "event,panes" {
		t.Fatalf("numeric resume = %v, %v", frames, err)
	}
}

func TestWatchEmptySnapshotFollowsFirstTimestampEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := eventlog.Open(t.TempDir())
	stop := errors.New("first event observed")
	s := watchStream{
		log: log,
		capture: func(ctx context.Context) (query.Snapshot, error) {
			return watchCapture(t, ctx, log, query.View{}, nil)
		},
		rescan: func(context.Context) ([]query.Pane, error) { return nil, nil },
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
	if err := s.run(ctx, -1, false); !errors.Is(err, stop) {
		t.Fatalf("empty startup = %v", err)
	}
}

func TestSendWaitHistoryLossRetainsAcceptedDeliveryOutcome(t *testing.T) {
	log := eventlog.Open(t.TempDir())
	log.MaxBytes = 1
	e := eventlog.Event{Event: eventlog.Send, Agent: "claude", Session: "session"}
	if err := log.Append(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	watchPulse(t, context.Background(), log, 4)
	var stdout, stderr bytes.Buffer
	a := app{log: log, json: true, stdout: &stdout, stderr: &stderr}
	code := a.awaitAndReportContext(context.Background(), "send", "%7", query.Session{Agent: "claude", ID: "session", State: "idle"}, query.WaitOptions{
		States: []string{"idle"}, After: e.Seq, Cursor: e.Cursor(), StartGrace: time.Second,
	}, time.Second)
	var d struct {
		Error   struct{ Code, Message string }
		Started *bool
	}
	if code != ExitError || json.Unmarshal(stdout.Bytes(), &d) != nil || d.Error.Code != "history_unavailable" || d.Started != nil {
		t.Fatalf("lost history became success/no-turn: %d %s", code, stdout.String())
	}
	if !strings.Contains(d.Error.Message, fmt.Sprintf("prompt delivered at seq %d", e.Seq)) || !strings.Contains(d.Error.Message, "do not repeat") {
		t.Fatalf("accepted delivery was lost: %s", d.Error.Message)
	}
	// What to do next names the target as typed, and keeps the prompt from
	// being sent twice.
	if want := nextPrefix + "the prompt arrived and its turn had not ended: run tmag wait %7 --for idle,blocked"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", stderr.String(), want)
	}
}

func TestReadCommandsCancelWhileLogLockHeld(t *testing.T) {
	// Inspect loads the world before reading history. Restrict that discovery
	// to a scratch tmux namespace, never the developer's server.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	for _, command := range []string{"list", "watch", "inspect", "wait", "state diagnostic"} {
		t.Run(command, func(t *testing.T) {
			log := eventlog.Open(t.TempDir())
			store := session.Store{Dir: log.Dir}
			if err := store.Put(session.Record{Agent: "claude", ID: "session", Main: "idle"}); err != nil {
				t.Fatal(err)
			}
			held, release := make(chan struct{}), make(chan struct{})
			go func() { _ = log.WithLock(context.Background(), func() error { close(held); <-release; return nil }) }()
			<-held
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			var stdout, stderr bytes.Buffer
			a := app{ctx: ctx, log: log, stdout: &stdout, stderr: &stderr}
			if command == "state diagnostic" {
				if got := a.checkState(ctx); got.Status != "fail" {
					t.Fatalf("diagnostic ignored canceled log lock: %+v", got)
				}
				return
			}
			args := []string{command}
			if command == "inspect" || command == "wait" {
				args = append(args, "session")
			}
			if command != "watch" {
				args = append(args, "--json")
			}
			if code := a.run(args); code != ExitTimeout || !strings.Contains(stdout.String(), `"code":"timeout"`) {
				t.Fatalf("%s ignored parent deadline: %d %s", command, code, stdout.String())
			}
		})
	}
}

func TestWatchInterruptionAfterSnapshotDoesNotEmitCopiedBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := eventlog.Open(t.TempDir())
	frames := 0
	s := watchStream{
		log: log,
		capture: func(ctx context.Context) (query.Snapshot, error) {
			return watchCapture(t, ctx, log, query.View{}, func() { watchPulse(t, ctx, log, 1) })
		},
		rescan: func(context.Context) ([]query.Pane, error) { return nil, nil },
		emit: func(doc map[string]any) error {
			frames++
			if doc["type"] != "snapshot" {
				t.Fatal("published backlog after interruption")
			}
			cancel()
			return nil
		},
	}
	if err := s.run(ctx, -1, false); !errors.Is(err, context.Canceled) || frames != 1 {
		t.Fatalf("interrupted handoff = %d frames, %v", frames, err)
	}
}
