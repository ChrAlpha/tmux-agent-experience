package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// Claude Code reports no hook when a turn is interrupted, but its status file
// turns idle. A read path applies tmag's idle transition from that file and
// logs it, so the session is listed idle without a Stop.
func TestClaudeStatusFileEndsAnUnreportedTurn(t *testing.T) {
	h := start(t)
	f := h.follow()
	out := filepath.Join(h.dir, "received")
	config := filepath.Join(h.dir, "claude-config")
	pane := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_KEYS": "1",
		"FAKE_STATUS": "1", "CLAUDE_CONFIG_DIR": config, "FAKE_TURN_MS": "60000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")

	if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "working")

	// The person interrupts, as Claude Code's Escape does: no hook fires.
	h.tmux("send-keys", "-t", pane, "Escape")
	h.eventually("the status file to idle the session", func() bool {
		s, ok := h.sessionOf(sidA)
		if !ok || s.State != "idle" || h.loggedState(sidA) != "idle" {
			return false
		}
		for _, e := range h.events(sidA) {
			if e.Event == "Idle" && e.State == "idle" {
				return true
			}
		}
		return false
	})
	if got := h.eventNames(sidA); strings.Contains(got, "Stop:") {
		t.Fatalf("a hook reported an interrupt: %s", got)
	}
	f.agrees(h)
}

// Compaction qualifies a working turn as compacting and ends where it began;
// a failed turn keeps the agent's reason, in the view and in its event.
func TestCompactionAndFailureReachTheView(t *testing.T) {
	h := start(t)
	h.externalHook(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "prompt_id": "p1", "cwd": h.dir})
	h.waitSession(sidA, "working")
	h.externalHook(map[string]any{"hook_event_name": "PreCompact", "session_id": sidA, "prompt_id": "p1", "trigger": "auto"})
	h.eventually("the automatic compaction to qualify the turn", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "working" && s.Kind == "compacting"
	})
	h.externalHook(map[string]any{"hook_event_name": "PostCompact", "session_id": sidA, "prompt_id": "p1", "trigger": "auto"})
	h.eventually("the turn to go on after compaction", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "working" && s.Kind == ""
	})
	h.externalHook(map[string]any{"hook_event_name": "Stop", "session_id": sidA, "prompt_id": "p1"})
	h.waitSession(sidA, "idle")

	// A manual compaction of an idle session works for it, then leaves it idle.
	h.externalHook(map[string]any{"hook_event_name": "PreCompact", "session_id": sidA, "prompt_id": "p2", "trigger": "manual"})
	h.eventually("the manual compaction to start work", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "working" && s.Kind == "compacting"
	})
	h.externalHook(map[string]any{"hook_event_name": "PostCompact", "session_id": sidA, "prompt_id": "p2", "trigger": "manual"})
	h.waitSession(sidA, "idle")

	h.externalHook(map[string]any{"hook_event_name": "StopFailure", "session_id": sidA, "prompt_id": "p3", "error": "model_not_found"})
	h.eventually("the failure reason in the view", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "idle" && s.Kind == "error" && s.Reason == "model_not_found"
	})
	events := h.events(sidA)
	last := events[len(events)-1]
	if last.Event != "StopFailure" || last.Kind != "error" || last.Reason != "model_not_found" {
		t.Fatalf("failure event = %+v", last)
	}
}

// A native Idle reached through a read path also projects the hinted pane,
// before a later no-op idle_prompt hook returns.
func TestNativeIdleProjectsHintedPane(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_KEYS": "1",
		"FAKE_STATUS": "1", "CLAUDE_CONFIG_DIR": filepath.Join(h.dir, "claude-config"), "FAKE_TURN_MS": "60000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	h.tmux("send-keys", "-t", pane, "long work", "Enter")
	h.eventually("working projection", func() bool {
		return h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}") == "working"
	})
	store := session.Store{Dir: h.state}
	before, ok, err := store.Get(context.Background(), "claude", sidA)
	if err != nil || !ok || !hasHint(before, pane) {
		t.Fatalf("no hint: %+v %v", before, err)
	}
	h.tmux("send-keys", "-t", pane, "Escape")
	h.waitSession(sidA, "idle") // list applies and projects the native Idle transition.
	after, ok, err := store.Get(context.Background(), "claude", sidA)
	if err != nil || !ok || !hasHint(after, pane) {
		t.Fatalf("lost hint: %+v %v", after, err)
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}"); got != "idle" {
		t.Fatalf("after list: record=%s pane=%s", after.State(), got)
	}
	// A later native idle_prompt is a no-op on the now-idle record; the pane
	// must stay idle after the synchronous notification hook returns.
	h.tmux("send-keys", "-t", pane, "/notify", "Enter")
	readEventually(t, out, "/notify")
	h.tmux("send-keys", "-t", pane, "/barrier", "Enter")
	readEventually(t, out, "/barrier")
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}"); got != "idle" {
		t.Fatalf("native Idle and subsequent idle_prompt left hinted pane %s; want idle", got)
	}
}

// A native Idle learned while an interrupt waits for its locks is applied and
// logged before the locked gate, so the interrupt refuses instead of pressing
// Escape on an idle agent.
func TestInterruptRechecksNativeIdle(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_KEYS": "1",
		"FAKE_STATUS": "1", "CLAUDE_CONFIG_DIR": filepath.Join(h.dir, "claude-config"), "FAKE_TURN_MS": "60000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 {
		t.Fatal(r)
	}
	s := h.waitSession(sidA, "working")
	store := session.Store{Dir: h.state}
	rec, ok, err := store.Get(context.Background(), "claude", sidA)
	if err != nil || !ok {
		t.Fatal(err)
	}
	query.BeforePaneLock = func() {
		err := attach.Lock(context.Background(), h.state, s.Panes[0].Server, pane, func() error {
			h.tmux("send-keys", "-t", pane, "Escape")
			h.eventually("native idle before the locked gate", func() bool {
				b, _ := os.ReadFile(rec.StatusFile)
				var status struct {
					Status  string `json:"status"`
					Updated int64  `json:"statusUpdatedAt"`
				}
				_ = json.Unmarshal(b, &status)
				return status.Status == "idle" && status.Updated > rec.LastHookAt.UnixMilli()
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() { query.BeforePaneLock = nil }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if d, err := query.Interrupt(ctx, eventlog.Open(h.state), sidA[:8]); err == nil {
		t.Fatalf("interrupt delivered Escape despite proven native idle before locked recheck: via=%s seq=%d", d.Via, d.Seq)
	}

	rec, ok, err = store.Get(ctx, "claude", sidA)
	if err != nil || !ok || rec.State() != "idle" {
		t.Fatalf("record not idle after the locked refusal: %+v %v", rec, err)
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}"); got != "idle" {
		t.Fatalf("the locked native Idle refusal left the hinted pane %s; want idle", got)
	}
	// A later idle_prompt and read must leave the pane idle, not project the
	// old working state back.
	h.tmux("send-keys", "-t", pane, "/notify", "Enter")
	readEventually(t, out, "/notify")
	h.tmux("send-keys", "-t", pane, "/barrier", "Enter")
	readEventually(t, out, "/barrier")
	h.waitSession(sidA, "idle")
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}"); got != "idle" {
		t.Fatalf("locked native Idle refusal plus idle_prompt and list left hinted pane %s; want idle", got)
	}
}
