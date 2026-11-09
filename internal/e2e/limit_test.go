package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// fill pads a session's record with a subagent id that is not running, which
// no event reports and no change clears, until the record is room bytes
// short of the record limit. It returns the record as stored.
func (h *harness) fill(agent, id string, room int) string {
	h.t.Helper()
	store := session.Store{Dir: h.state}
	path := filepath.Join(h.state, "sessions", agent, id+".json")
	err := store.Lock(context.Background(), agent, id, func() error {
		rec, ok, err := store.Get(agent, id)
		if err != nil {
			return fmt.Errorf("record of %s: %w", id, err)
		}
		if !ok {
			return fmt.Errorf("no record of %s", id)
		}
		rec.Subagents = append(rec.Subagents, "x")
		if err := store.Put(rec); err != nil {
			return err
		}
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		rec.Subagents[len(rec.Subagents)-1] = strings.Repeat("x", 1+session.MaxRecordBytes-room-int(st.Size()))
		return store.Put(rec)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) != session.MaxRecordBytes-room {
		h.t.Fatalf("filled record is %d bytes: %v", len(data), err)
	}
	return string(data)
}

func (h *harness) head() eventlog.Cursor {
	h.t.Helper()
	c, err := eventlog.Open(h.state).Head(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// unchanged fails unless the session's record and the log are as they were.
func (h *harness) unchanged(agent, id, record string, head eventlog.Cursor) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.state, "sessions", agent, id+".json"))
	if err != nil || string(data) != record {
		h.t.Fatalf("the record changed: %v", err)
	}
	if now := h.head(); now != head {
		h.t.Fatalf("the log moved: %+v -> %+v", head, now)
	}
}

func refusedForLimit(t *testing.T, r result) {
	t.Helper()
	if r.code != 1 || !strings.Contains(r.out, `"code":"limit_exceeded"`) {
		t.Fatalf("exit %d: %s", r.code, r.out)
	}
}

// A prompt whose record would exceed the record limit is refused before it
// is typed.
func TestSendRefusedBeforeTypingAtTheRecordLimit(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	// The sent mark takes about thirty bytes.
	record := h.fill("claude", sidA, 10)
	head := h.head()
	refusedForLimit(t, h.tmag("", "send", h.target(pane), "--json", "hello"))
	time.Sleep(300 * time.Millisecond)
	if got, _ := os.ReadFile(out); strings.Contains(string(got), "hello") {
		t.Fatalf("the agent received %q", got)
	}
	h.unchanged("claude", sidA, record, head)
}

// An interrupt whose record would exceed the record limit is refused before
// Escape is pressed: Claude Code reports nothing of an interrupt, so tmag
// records the idle session and the interrupt with it.
func TestInterruptRefusedBeforeEscapeAtTheRecordLimit(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_KEYS": "1", "FAKE_TURN_MS": "20000"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	if r := h.tmag("", "send", h.target(pane), "long work"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitPane(pane, "working", "process")
	// The halted mark alone fits; marked, the record also keeps the Halt.
	record := h.fill("claude", sidA, 40)
	head := h.head()
	refusedForLimit(t, h.tmag("", "interrupt", h.target(pane), "--json"))
	time.Sleep(500 * time.Millisecond)
	if got, _ := os.ReadFile(out); string(got) != "long work\n" {
		t.Fatalf("the agent received %q", got)
	}
	h.unchanged("claude", sidA, record, head)
	if s, _ := h.sessionOf(sidA); s.State != "working" {
		t.Fatalf("session after a refused interrupt: %+v", s)
	}
}

// Through the Codex daemon, a prompt or an answer whose record would exceed
// the record limit is refused before the daemon gets it.
func TestCodexDaemonRefusesBeforeDeliveryAtTheRecordLimit(t *testing.T) {
	h := start(t)
	d := h.daemon()
	d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": sidC, "source": "startup"})
	h.waitSession(sidC, "idle")
	record := h.fill("codex", sidC, 10)
	head := h.head()
	refusedForLimit(t, h.tmag("", "send", sidC[:8], "--json", "hello"))
	if got, _ := os.ReadFile(d.out); strings.Contains(string(got), "hello") {
		t.Fatalf("the daemon started %q", got)
	}
	h.unchanged("codex", sidC, record, head)

	d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": sidD, "source": "startup"})
	d.hook(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidD, "turn_id": "t1"})
	d.hook(map[string]any{"hook_event_name": "PreToolUse", "session_id": sidD, "turn_id": "t1", "tool_name": "Bash", "tool_use_id": "call-1"})
	d.hook(map[string]any{"hook_event_name": "PermissionRequest", "session_id": sidD, "turn_id": "t1", "tool_name": "Bash"})
	s := h.waitSession(sidD, "blocked")
	if len(s.Requests) != 1 {
		t.Fatalf("blocked session = %+v", s)
	}
	// Marked, the record keeps when the request was taken and the Answer.
	record = h.fill("codex", sidD, 50)
	head = h.head()
	refusedForLimit(t, h.tmag("", "answer", sidD[:8], s.Requests[0].ID, "allow", "--json"))
	time.Sleep(300 * time.Millisecond)
	h.unchanged("codex", sidD, record, head)
	if s, _ := h.sessionOf(sidD); s.State != "blocked" {
		t.Fatalf("session after a refused answer: %+v", s)
	}
}

// A hook whose change would exceed the record limit fails open: it prints
// nothing, exits 0, changes nothing, and logs only its failure's category.
func TestHookRefusesAChangeTooLargeToRecord(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	record, err := os.ReadFile(filepath.Join(h.state, "sessions", "claude", sidA+".json"))
	if err != nil {
		t.Fatal(err)
	}
	head := h.head()
	h.externalHook(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "prompt_id": "p9",
		"cwd": strings.Repeat("<", session.MaxRecordBytes/6+1)})
	h.unchanged("claude", sidA, string(record), head)
	log, err := os.ReadFile(filepath.Join(h.state, "hook.log"))
	if err != nil || !strings.Contains(string(log), " claude limit_exceeded\n") || strings.Contains(string(log), "<") {
		t.Fatalf("hook.log %q: %v", log, err)
	}
}

// Options that mark an attachment change tmag cannot read are left as they
// are, with nothing logged and no other attachment written over them:
// reconciliation reports them instead.
func TestUnreadableAttachmentMarkersArePreserved(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	srv := tmux.ServerByName(h.name)
	ctx := context.Background()
	var format []string
	for _, k := range tmux.OptionKeys {
		format = append(format, "#{"+tmux.OptionPrefix+k+"}")
	}
	options := func() string { return h.tmux("display-message", "-p", "-t", pane, strings.Join(format, "|")) }
	set := func(values map[string]string, unset ...string) {
		t.Helper()
		if err := attach.Lock(ctx, h.state, srv.Socket, pane, func() error {
			return srv.SetPaneOptions(ctx, pane, values, unset)
		}); err != nil {
			t.Fatal(err)
		}
	}
	valid := options()
	for name, spoil := range map[string]map[string]string{
		"replaced":     {"change": eventlog.Attach, "replaced": "claude ../" + sidB},
		"change":       {"change": "Bogus"},
		"main session": {"change": eventlog.Attach, "session": "../" + sidA},
	} {
		set(spoil)
		marked, head := options(), h.head()
		r := h.tmag("", "status")
		if r.code != 1 || !strings.Contains(r.out, "attachment_unavailable") || strings.Contains(r.out, "../") {
			t.Fatalf("%s: status exited %d: %s", name, r.code, r.out)
		}
		if got := options(); got != marked {
			t.Fatalf("%s: options %q, were %q", name, got, marked)
		}
		if now := h.head(); now != head {
			t.Fatalf("%s: the log moved", name)
		}
		set(map[string]string{"session": sidA}, "change", "replaced")
		if got := options(); got != valid {
			t.Fatalf("%s: restored options %q, want %q", name, got, valid)
		}
	}
	if r := h.tmag("", "status"); r.code != 0 {
		t.Fatalf("status with a readable attachment: %d %s", r.code, r.out)
	}
}
