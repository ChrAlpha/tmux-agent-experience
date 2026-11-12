package e2e

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// attachClient attaches a script client to main and returns its input. It
// reports false on platforms without util-linux script.
func (h *harness) attachClient(t *testing.T) (io.WriteCloser, bool) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Log("attaching a client needs util-linux script")
		return nil, false
	}
	client := exec.Command("script", "-qfc", "tmux -L "+h.name+" attach -t main", "/dev/null")
	client.Env = append(os.Environ(), "TERM=xterm-256color")
	keys, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Process.Kill()
		_ = client.Wait()
	})
	h.eventually("an attached client", func() bool { return h.tmux("list-clients") != "" })
	return keys, true
}

// The terminal's view settles turns: a finished turn in a pane nobody watches
// stays a jump target, and selecting the pane while the person is at the
// client marks it seen. A turn that ends in the watched pane is seen at once.
func TestSeenFollowsTheTerminal(t *testing.T) {
	h := start(t)
	h.waitBinary(h.loadPlugin(false, nil))
	f := h.follow()
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	sock := h.tmux("display-message", "-p", "#{socket_path}")

	// A turn ends with no client attached.
	h.tmux("send-keys", "-t", pane, "hello", "Enter")
	h.eventually("the turn ends", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "idle" && s.Turn != ""
	})
	s, _ := h.sessionOf(sidA)
	turn := s.Turn
	if s.Seen != "" {
		t.Fatalf("an unwatched turn was seen: %+v", s)
	}
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 finished" {
		t.Fatalf("status = %q", r.out)
	}
	if r := h.tmag("", "jump", "--server", sock); r.code != 0 || strings.TrimSpace(r.out) != sock+":"+pane {
		t.Fatalf("jump = %d %q, want %s", r.code, r.out, pane)
	}

	// The person attaches, types, and selects the agent's window.
	keys, ok := h.attachClient(t)
	if !ok {
		return
	}
	if _, err := io.WriteString(keys, " "); err != nil {
		t.Fatal(err)
	}
	h.tmux("select-window", "-t", pane)
	h.eventually("the turn seen at the terminal", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.Seen == turn
	})
	seenEvents := func() int {
		n := 0
		for _, e := range h.events(sidA) {
			if e.Event == eventlog.Seen && e.Turn == turn {
				n++
			}
		}
		return n
	}
	if n := seenEvents(); n != 1 {
		t.Fatalf("%d Seen events for the turn", n)
	}
	if r := h.tmag("", "jump", "--server", sock); r.code == 0 {
		t.Fatalf("a seen turn is still a jump target: %s", r.out)
	}
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 idle" {
		t.Fatalf("status after seen = %q", r.out)
	}
	f.agrees(h)

	// The next turn ends in the pane the client is watching.
	if r := h.tmag("", "send", sidA[:8], "--wait", "again"); r.code != 0 {
		t.Fatalf("send --wait: %d %s", r.code, r.out)
	}
	s, _ = h.sessionOf(sidA)
	if s.Turn == turn || s.Seen != s.Turn {
		t.Fatalf("a turn that ended in the watched pane is %+v", s)
	}
	f.agrees(h)

	// last-pane (prefix + ;) is a window pane change too: a turn that ends
	// while the new shell pane is active is unseen until the agent's pane
	// comes back into view.
	if got := h.tmux("split-window", "-t", pane, "-P", "-F", "#{pane_id}", "/bin/sh"); got == "" {
		t.Fatal("split-window returned no pane")
	}
	if r := h.tmag("", "send", sidA[:8], "--wait", "last pane"); r.code != 0 {
		t.Fatalf("send --wait: %d %s", r.code, r.out)
	}
	s, _ = h.sessionOf(sidA)
	if s.Seen == s.Turn {
		t.Fatalf("a turn in the hidden pane was seen: %+v", s)
	}
	h.tmux("last-pane", "-t", pane)
	h.eventually("seen after last-pane", func() bool {
		got, ok := h.sessionOf(sidA)
		return ok && got.Seen == s.Turn
	})
	f.agrees(h)

	// An agent never marks a turn seen, even its own.
	if r := h.tmag("", "send", sidA[:8], "--wait", "a tool"); r.code != 0 {
		t.Fatalf("send --wait: %d %s", r.code, r.out)
	}
	s, _ = h.sessionOf(sidA)
	h.tmux("send-keys", "-t", pane, "tmag seen "+sidA+" --turn "+s.Turn, "Enter")
	h.eventually("the agent's refusal", func() bool {
		got, _ := os.ReadFile(out)
		return strings.Contains(string(got), "tool 3 tmag seen: refused (agent)")
	})
	if got, _ := h.sessionOf(sidA); got.Seen != s.Turn {
		t.Fatalf("an agent changed seen: %+v", got)
	}
	f.agrees(h)
}

// The status line runs for an attached client, so a finished turn in the
// pane that client watches is seen once it is drawn, unless @tmag-seen is off.
func TestStatusMarksTheViewSeen(t *testing.T) {
	h := start(t)
	sock := h.tmux("display-message", "-p", "#{socket_path}")
	h.tmux("set-option", "-g", "@tmag-seen", "off")
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	h.tmux("send-keys", "-t", pane, "hello", "Enter")
	h.eventually("the turn ends", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "idle" && s.Turn != ""
	})
	s, _ := h.sessionOf(sidA)
	if s.Seen != "" {
		t.Fatalf("@tmag-seen off marked a turn: %+v", s)
	}
	keys, ok := h.attachClient(t)
	if !ok {
		return
	}
	if _, err := io.WriteString(keys, " "); err != nil {
		t.Fatal(err)
	}
	h.tmux("select-window", "-t", pane)
	time.Sleep(200 * time.Millisecond) // a view change would have run the hook
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 finished" {
		t.Fatalf("status with @tmag-seen off = %q", r.out)
	}
	if got, _ := h.sessionOf(sidA); got.Seen != "" {
		t.Fatalf("@tmag-seen off marked a turn: %+v", got)
	}
	h.tmux("set-option", "-g", "@tmag-seen", "on")
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 idle" {
		t.Fatalf("status with @tmag-seen on = %q", r.out)
	}
	if got, _ := h.sessionOf(sidA); got.Seen != s.Turn {
		t.Fatalf("status did not mark the view seen: %+v", got)
	}
}

// A view read before a turn ended cannot mark it: the watched pane and the
// server's presence are observed again under the session's lock.
func TestStaleViewDoesNotMarkAnUnreadTurn(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out, "FAKE_TURN_MS": "3000"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	keys, ok := h.attachClient(t)
	if !ok {
		return
	}
	if _, err := io.WriteString(keys, " "); err != nil {
		t.Fatal(err)
	}
	h.tmux("select-window", "-t", pane)
	if r := h.tmux("display-message", "-p", "-t", pane, "#{pane_active}"); r != "1" {
		t.Fatalf("the agent pane is not active: %s", r)
	}

	// A turn starts while the person watches it; the view is read then.
	h.tmux("send-keys", "-t", pane, "work", "Enter")
	h.eventually("the turn to run", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "working"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := session.Store{Dir: h.state}
	before, stale, err := query.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	working, ok := before.Session("claude", sidA)
	if !ok || working.State != "working" || working.Turn == "" {
		t.Fatalf("the view before the end: %+v", working)
	}
	watched := false
	for _, p := range stale.Panes {
		watched = watched || (p.ID == pane && p.Watched)
	}
	if !watched {
		t.Fatalf("the stale view does not show %s watched", pane)
	}

	// The person looks away, and the turn ends unseen.
	h.tmux("select-window", "-t", "main:0")
	h.eventually("the turn to end", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "idle" && s.Turn == working.Turn
	})
	after, _, err := query.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	// A person runs the automatic path: a world whose process table proves no
	// agent above the test, with the stale view's panes.
	caller := &attach.World{Panes: stale.Panes, Proc: proc.NewTable([]proc.Process{{PID: os.Getpid(), PPID: 1, Comm: "go"}})}
	marked, err := query.MarkShown(ctx, eventlog.Open(h.state), after, caller, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("a stale view marked %+v", marked)
	}
	if s, _ := h.sessionOf(sidA); s.Seen == s.Turn {
		t.Fatalf("a stale view marked the turn seen: %+v", s)
	}
	for _, e := range h.events(sidA) {
		if e.Event == eventlog.Seen {
			t.Fatalf("a stale view logged %+v", e)
		}
	}
}

// An agent never marks a turn through status or seen --shown, even with a
// watched pane and recent input.
func TestAgentsMarkNoTurnsAutomatically(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "0", "FAKE_OUT": out}, filepath.Join(bin, "claude"))
	pidText := h.tmux("display-message", "-p", "-t", pane, "#{pane_pid}")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("pane pid %q: %v", pidText, err)
	}
	started, err := proc.SnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, found := started.Get(pid)
	if !found {
		t.Fatalf("no process %d", pid)
	}
	// The fake reports no hooks: the record is the one written here, and the
	// pane options are its proof.
	rec := session.Record{
		Agent: proc.Claude, ID: sidA, Main: adapter.Idle, Turn: "turn-agent", Cwd: h.dir,
		Host:  session.Host{PID: p.PID, Start: p.Start, Role: proc.Terminal},
		Since: time.Now().Add(-time.Minute).UTC().Truncate(time.Second),
	}
	if err := (session.Store{Dir: h.state}).Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"@tmag_agent": "claude", "@tmag_session": sidA, "@tmag_pid": pidText, "@tmag_start": p.Start,
		"@tmag_evidence": "process", "@tmag_state": "idle", "@tmag_cmd": "claude",
	} {
		h.tmux("set-option", "-p", "-t", pane, k, v)
	}
	keys, ok := h.attachClient(t)
	if !ok {
		return
	}
	if _, err := io.WriteString(keys, " "); err != nil {
		t.Fatal(err)
	}
	h.tmux("select-window", "-t", pane)

	// status is the agent's tool: it prints the count and marks nothing.
	h.tmux("send-keys", "-t", pane, "tmag status", "Enter")
	h.eventually("the agent's status", func() bool {
		b, _ := os.ReadFile(out)
		return strings.Contains(string(b), "tool 0 1 finished")
	})
	if s, _ := h.sessionOf(sidA); s.Seen == s.Turn {
		t.Fatalf("status marked the turn: %+v", s)
	}
	// seen --shown is refused, with the same gate as seen ID.
	h.tmux("send-keys", "-t", pane, "tmag seen --shown", "Enter")
	h.eventually("the agent's seen refusal", func() bool {
		b, _ := os.ReadFile(out)
		return strings.Contains(string(b), "tool 3 tmag seen: refused (agent)") &&
			strings.Contains(string(b), "next: only the person can mark a turn seen")
	})
	if s, _ := h.sessionOf(sidA); s.Seen == s.Turn {
		t.Fatalf("seen --shown marked the turn: %+v", s)
	}
}

// A stale explicit mark is refused: once the session is working on another
// turn, or idle on it, a turn it has left is not a quiet success.
func TestSeenRefusesATurnTheSessionLeft(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "1500"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	h.tmux("send-keys", "-t", pane, "first", "Enter")
	h.eventually("the first turn", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "idle" && s.Turn != ""
	})
	s, _ := h.sessionOf(sidA)
	seen := s.Turn
	if r := h.tmag("", "seen", sidA[:8], "--turn", seen); r.code != 0 {
		t.Fatalf("seen the first turn: %d %s", r.code, r.out)
	}

	// The next turn starts; the old turn is not idle any more.
	h.tmux("send-keys", "-t", pane, "second", "Enter")
	h.eventually("the second turn to run", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "working" && s.Turn != seen
	})
	if r := h.tmag("", "seen", sidA[:8], "--turn", seen); r.code != 3 || !strings.Contains(r.out, "busy") || !strings.Contains(r.out, "next:") {
		t.Fatalf("a seen turn while the next works: %d %s", r.code, r.out)
	}
	h.eventually("the second turn to end", func() bool {
		s, ok := h.sessionOf(sidA)
		return ok && s.State == "idle" && s.Turn != seen
	})
	if r := h.tmag("", "seen", sidA[:8], "--turn", seen); r.code != 3 || !strings.Contains(r.out, "changed") || !strings.Contains(r.out, "next:") {
		t.Fatalf("a seen turn after the next finished: %d %s", r.code, r.out)
	}
	if got, _ := h.sessionOf(sidA); got.Seen != seen {
		t.Fatalf("a refused mark changed seen: %+v", got)
	}
	for _, e := range h.events(sidA) {
		if e.Event == eventlog.Seen && e.Turn != seen {
			t.Fatalf("a refused mark logged %+v", e)
		}
	}
}

// A watched pane whose projection still names a session proves nothing once
// its title shows another thread: the attachment is proven again from a fresh
// attach world, as list and jump prove it.
func TestStaleTitleDoesNotMarkAnUnreadTurn(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	d := h.daemon()
	pane := h.spawn(repo, map[string]string{"FAKE_TITLE": codexTitle("A", sidA)}, filepath.Join(bin, "codex"))
	h.eventually("the codex pane", func() bool {
		_, _, ok := h.paneIn(pane)
		return ok
	})
	d.hook(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "cwd": repo, "turn_id": "tA"})
	d.hook(map[string]any{"hook_event_name": "Stop", "session_id": sidA, "cwd": repo, "turn_id": "tA"})
	h.waitSession(sidA, "idle")
	keys, ok := h.attachClient(t)
	if !ok {
		return
	}
	if _, err := io.WriteString(keys, " "); err != nil {
		t.Fatal(err)
	}
	h.tmux("select-window", "-t", pane)
	if r := h.tmag("", "prune"); r.code != 0 {
		t.Fatalf("prune: %d %s", r.code, r.out)
	}
	if _, s := h.waitPane(pane, "idle", "title-id"); s.ID != sidA {
		t.Fatalf("the pane shows %+v", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := session.Store{Dir: h.state}
	before, stale, err := query.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	showing, ok := before.Session(proc.Codex, sidA)
	if !ok || showing.State != "idle" || showing.Turn != "tA" || showing.Seen != "" || len(showing.Panes) == 0 {
		t.Fatalf("the view before the switch: %+v", showing)
	}
	watched := false
	for _, p := range stale.Panes {
		watched = watched || (p.ID == pane && p.Watched)
	}
	if !watched {
		t.Fatalf("the stale view does not show %s watched", pane)
	}

	// The title moves to another thread; the pane's projection still names A.
	h.tmux("select-pane", "-t", pane, "-T", codexTitle("B", sidB))
	after, _, err := query.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := after.Session(proc.Codex, sidA); ok && len(s.Panes) != 0 {
		t.Fatalf("a fresh view still attaches A: %+v", s)
	}
	caller := &attach.World{Proc: proc.NewTable([]proc.Process{{PID: os.Getpid(), PPID: 1, Comm: "go"}})}
	marked, err := query.MarkShown(ctx, eventlog.Open(h.state), before, caller, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("a stale title marked %+v", marked)
	}
	if s, ok := h.sessionOf(sidA); ok && s.Seen != "" {
		t.Fatalf("a stale title marked the turn seen: %+v", s)
	}
	for _, e := range h.events(sidA) {
		if e.Event == eventlog.Seen {
			t.Fatalf("a stale title logged %+v", e)
		}
	}
}

// A Codex parent's recorded subagent must not make a pane that proves the
// subagent's own session look like the parent: the proof is built from all
// live sessions, as query.Load builds it.
func TestCodexParentDoesNotClaimTheSubagentsPane(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	d := h.daemon()
	// The child session is live before the parent records it: the pane's
	// stored attachment to B is written while no one else claims B.
	d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": sidB, "cwd": repo})
	pane := h.spawn(repo, map[string]string{"FAKE_TITLE": codexTitle("B", sidB)}, filepath.Join(bin, "codex"))
	h.eventually("the codex pane", func() bool {
		_, _, ok := h.paneIn(pane)
		return ok
	})
	h.tmux("select-pane", "-t", pane, "-T", codexTitle("B", sidB))
	if r := h.tmag("", "prune"); r.code != 0 {
		t.Fatalf("prune: %d %s", r.code, r.out)
	}
	if _, s := h.waitPane(pane, "idle", "title-id"); s.ID != sidB {
		t.Fatalf("the pane shows %+v", s)
	}
	keys, ok := h.attachClient(t)
	if !ok {
		return
	}
	if _, err := io.WriteString(keys, " "); err != nil {
		t.Fatal(err)
	}
	h.tmux("select-window", "-t", pane)

	// The parent records B as a subagent and ends its unread turn.
	d.hook(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "cwd": repo, "turn_id": "tA"})
	d.hook(map[string]any{"hook_event_name": "SubagentStart", "session_id": sidA, "cwd": repo, "turn_id": "tA", "agent_id": sidB})
	d.hook(map[string]any{"hook_event_name": "Stop", "session_id": sidA, "cwd": repo, "turn_id": "tA"})
	d.hook(map[string]any{"hook_event_name": "SubagentStop", "session_id": sidA, "cwd": repo, "turn_id": "tA", "agent_id": sidB})
	if s := h.waitSession(sidA, "idle"); s.Turn != "tA" {
		t.Fatalf("the parent is %+v", s)
	}
	parent, ok, err := (session.Store{Dir: h.state}).Get(context.Background(), proc.Codex, sidA)
	if err != nil || !ok || !slices.Contains(parent.Subagents, sidB) || len(parent.Background) != 0 || !parent.Finished() {
		t.Fatalf("the parent record = %+v, %v, %v", parent, ok, err)
	}

	// The title shows the parent, so the stale view gives it the pane; the
	// stored attachment still proves B.
	h.tmux("select-pane", "-t", pane, "-T", codexTitle("A", sidA))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := session.Store{Dir: h.state}
	before, stale, err := query.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	showing, ok := before.Session(proc.Codex, sidA)
	if !ok || showing.State != "idle" || showing.Turn != "tA" || showing.Seen != "" || len(showing.Panes) == 0 {
		t.Fatalf("the parent before the title moves: %+v", showing)
	}
	watched := false
	for _, p := range stale.Panes {
		watched = watched || (p.ID == pane && p.Watched)
	}
	if !watched {
		t.Fatalf("the stale view does not show %s watched", pane)
	}

	// The title moves back to B: a fresh view gives the pane to B, not A.
	h.tmux("select-pane", "-t", pane, "-T", codexTitle("B", sidB))
	after, _, err := query.Load(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := after.Session(proc.Codex, sidA); ok && len(s.Panes) != 0 {
		t.Fatalf("a fresh view still attaches the parent: %+v", s)
	}
	caller := &attach.World{Proc: proc.NewTable([]proc.Process{{PID: os.Getpid(), PPID: 1, Comm: "go"}})}
	marked, err := query.MarkShown(ctx, eventlog.Open(h.state), before, caller, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("the parent's subagent marked the parent: %+v", marked)
	}
	if s, ok := h.sessionOf(sidA); ok && s.Seen != "" {
		t.Fatalf("the parent's subagent marked the parent seen: %+v", s)
	}
	for _, e := range h.events(sidA) {
		if e.Event == eventlog.Seen {
			t.Fatalf("the parent's subagent logged %+v", e)
		}
	}
}
