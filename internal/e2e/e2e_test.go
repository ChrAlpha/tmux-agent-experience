// Package e2e drives the tmag binary against real, isolated tmux servers with
// fake agents. Skipped when tmux is not installed.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/registry"
)

var bin string // directory holding tmag, claude and codex

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Println("tmux not installed; skipping e2e tests")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "tmag-e2e-bin")
	if err != nil {
		panic(err)
	}
	bin = dir
	build := func(pkg, out string) {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, out), pkg)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			panic(err)
		}
	}
	build("github.com/ChrAlpha/tmux-agent-experience/cmd/tmag", "tmag")
	build("github.com/ChrAlpha/tmux-agent-experience/internal/e2e/fakeagent", "claude")
	if err := os.Link(filepath.Join(dir, "claude"), filepath.Join(dir, "codex")); err != nil {
		panic(err)
	}
	// Never touch the developer's own tmux server.
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type harness struct {
	t     *testing.T
	name  string // tmux -L socket name
	dir   string
	state string
}

func start(t *testing.T) *harness {
	t.Helper()
	// Socket paths are limited to ~104 bytes; keep the tmpdir short.
	sock, err := os.MkdirTemp("/tmp", "tm")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, name: "e2e", dir: t.TempDir(), state: filepath.Join(sock, "state")}
	t.Setenv("TMUX_TMPDIR", sock)
	t.Setenv("TMAG_STATE_DIR", h.state)
	t.Setenv("CODEX_HOME", filepath.Join(h.dir, "codex-home"))
	if out, err := exec.Command("tmux", "-L", h.name, "-f", "/dev/null", "new-session", "-d", "-s", "main", "-x", "200", "-y", "50").CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", h.name, "kill-server").Run() // may already be gone
		os.RemoveAll(sock)
	})
	return h
}

func (h *harness) tmux(args ...string) string {
	h.t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", h.name}, args...)...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("tmux %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// target addresses a pane of this harness's server from outside tmux.
func (h *harness) target(pane string) string { return h.name + ":" + pane }

// spawn runs argv in a new window and returns the pane id.
func (h *harness) spawn(cwd string, env map[string]string, argv ...string) string {
	h.t.Helper()
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-c", cwd}
	env["TMAG_BIN"] = filepath.Join(bin, "tmag")
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	return h.tmux(append(args, argv...)...)
}

type result struct {
	out  string
	code int
}

func (h *harness) tmag(stdin string, args ...string) result {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(bin, "tmag"), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return result{out: stdout.String() + stderr.String(), code: code}
}

func (h *harness) agents() []query.Agent {
	h.t.Helper()
	r := h.tmag("", "list", "--json")
	var doc struct {
		Schema string        `json:"schema"`
		Agents []query.Agent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(r.out), &doc); err != nil || doc.Schema != "tmag/v1" {
		h.t.Fatalf("list --json: %v %q", err, r.out)
	}
	return doc.Agents
}

func (h *harness) agentIn(pane string) (query.Agent, bool) {
	for _, a := range h.agents() {
		if a.Pane.ID == pane {
			return a, true
		}
	}
	return query.Agent{}, false
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; agents: %+v", what, h.agents())
}

func (h *harness) waitState(pane, state, binding string) query.Agent {
	h.t.Helper()
	var got query.Agent
	h.eventually(fmt.Sprintf("%s to be %s/%s", pane, state, binding), func() bool {
		a, ok := h.agentIn(pane)
		got = a
		return ok && a.State == state && a.Binding == binding
	})
	return got
}

func (h *harness) events(session string) []eventlog.Event {
	h.t.Helper()
	all, _, err := eventlog.Open(h.state).Since(context.Background(), 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []eventlog.Event
	for _, e := range all {
		if e.Session == session {
			out = append(out, e)
		}
	}
	return out
}

// externalHook runs a hook from outside any pane, as Codex's shared daemon does.
func (h *harness) externalHook(agent string, payload map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(payload)
	if r := h.tmag(string(b), "hook", agent); r.code != 0 || r.out != "" {
		h.t.Fatalf("hook: code %d output %q", r.code, r.out)
	}
}

const (
	sidA = "0a000000-0000-7000-8000-00000000000a"
	sidB = "0b000000-0000-7000-8000-00000000000b"
	sidC = "0c000000-0000-7000-8000-00000000000c"
	sidD = "0d000000-0000-7000-8000-00000000000d"
	sidE = "0e000000-0000-7000-8000-00000000000e"
)

func TestClaudeInProcess(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "300"}, filepath.Join(bin, "claude"))
	a := h.waitState(pane, "idle", "process")
	if a.ID != sidA || a.Agent != "claude" || a.Pane.Command != "claude" {
		t.Fatalf("agent = %+v", a)
	}

	r := h.tmag("", "send", sidA[:8], "--wait", "--json", "review", "the", "diff")
	if r.code != 0 || !strings.Contains(r.out, `"state":"idle"`) {
		t.Fatalf("send --wait: %d %s", r.code, r.out)
	}
	if got, _ := os.ReadFile(out); string(got) != "review the diff\n" {
		t.Fatalf("agent received %q", got)
	}

	var kinds []string
	for _, e := range h.events(sidA) {
		kinds = append(kinds, e.Event+":"+e.State)
		if e.Binding != "process" {
			t.Errorf("event %s bound by %s", e.Event, e.Binding)
		}
	}
	want := "SessionStart:idle Send: UserPromptSubmit:working Stop:idle"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}

	// A second prompt while the first is running is refused, not queued.
	if r := h.tmag("first", "send", h.target(pane)); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "send", h.target(pane), "second"); r.code != 3 || !strings.Contains(r.out, "busy") {
		t.Fatalf("busy send: %d %s", r.code, r.out)
	}
	h.waitState(pane, "idle", "process")
	if r := h.tmag("", "wait", h.target(pane), "--json"); r.code != 0 || !strings.Contains(r.out, `"idle"`) {
		t.Fatalf("wait on idle agent: %d %s", r.code, r.out)
	}

	// A prompt that starts no turn (a slash command) releases the agent.
	r = h.tmag("", "send", h.target(pane), "--wait", "--json", "/model")
	if r.code != 0 || !strings.Contains(r.out, `"started":false`) {
		t.Fatalf("send slash command --wait: %d %s", r.code, r.out)
	}
	h.waitState(pane, "idle", "process")

	// A waiter learns that the session ended instead of timing out.
	waiter := exec.Command(filepath.Join(bin, "tmag"), "wait", sidA[:8], "--for", "blocked", "--timeout", "20s", "--json")
	var wout bytes.Buffer
	waiter.Stdout = &wout
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Ending the session clears the pane record.
	h.tmux("send-keys", "-t", pane, "C-d")
	h.eventually("session end", func() bool {
		evs := h.events(sidA)
		return len(evs) > 0 && evs[len(evs)-1].Event == "SessionEnd"
	})
	err := waiter.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(wout.String(), `"ended"`) {
		t.Fatalf("waiter: %v %s", err, wout.String())
	}
}

func TestUnboundAgentIsVisibleButNotControllable(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	a := h.waitState(pane, "unknown", "unbound")
	if a.ID != "" {
		t.Fatalf("unbound agent has id %q", a.ID)
	}
	if r := h.tmag("", "send", h.target(pane), "hello"); r.code != 3 || !strings.Contains(r.out, "unbound") {
		t.Fatalf("send to unbound: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "read", h.target(pane), "--lines", "5"); r.code != 0 || !strings.Contains(r.out, "ready") {
		t.Fatalf("read: %d %q", r.code, r.out)
	}
}

func writeIndex(t *testing.T, names map[string]string) {
	t.Helper()
	home := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for id, name := range names {
		fmt.Fprintf(&b, `{"id":%q,"thread_name":%q,"updated_at":"2026-11-03T00:00:00Z"}`+"\n", id, name)
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexDaemonBinding(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	other := filepath.Join(h.dir, "other")
	for _, dir := range []string{repo, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeIndex(t, map[string]string{sidB: "Refactor parser module", sidC: "Shared name", sidD: "Shared name"})

	named := h.spawn(repo, map[string]string{"FAKE_TITLE": "⠋ Refactor parser module | repo"}, filepath.Join(bin, "codex"))
	byID := h.spawn(other, map[string]string{"FAKE_TITLE": sidE[:29] + "... Working"}, filepath.Join(bin, "codex"))
	shared := h.spawn(repo, map[string]string{"FAKE_TITLE": "Shared name"}, filepath.Join(bin, "codex"))
	h.waitState(named, "unknown", "unbound")
	h.waitState(shared, "unknown", "unbound")

	h.externalHook("codex", map[string]any{"hook_event_name": "Stop", "session_id": sidB, "cwd": repo, "turn_id": "t1"})
	if a := h.waitState(named, "idle", "title-name"); a.ID != sidB || a.Turn != "t1" {
		t.Fatalf("named pane = %+v", a)
	}

	h.externalHook("codex", map[string]any{"hook_event_name": "PermissionRequest", "session_id": sidE, "cwd": repo, "turn_id": "t9"})
	if a := h.waitState(byID, "blocked", "title-id"); a.Kind != "permission" {
		t.Fatalf("title-id pane = %+v", a)
	}

	// A name shared by two threads proves nothing.
	h.externalHook("codex", map[string]any{"hook_event_name": "Stop", "session_id": sidC, "cwd": repo, "turn_id": "t1"})
	evs := h.events(sidC)
	if len(evs) != 1 || evs[0].Binding != "unbound" || evs[0].Pane != nil {
		t.Fatalf("shared-name events = %+v", evs)
	}
	if a, _ := h.agentIn(shared); a.Binding != "unbound" {
		t.Fatalf("shared pane bound: %+v", a)
	}

	// Later events follow the established binding even if the title changes.
	h.tmux("select-pane", "-t", named, "-T", "something else")
	h.externalHook("codex", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidB, "cwd": repo, "turn_id": "t2"})
	if a := h.waitState(named, "working", "session"); a.Turn != "t2" {
		t.Fatalf("session-bound pane = %+v", a)
	}
}

func TestDaemonInsidePaneDoesNotBind(t *testing.T) {
	h := start(t)
	// A Codex daemon started from a pane runs hooks for every thread; its
	// pane ancestry must not attribute them to that pane.
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidD, "FAKE_HOOKS": "1"}, filepath.Join(bin, "codex"), "app-server", "--managed-daemon")
	h.eventually("daemon hook", func() bool { return len(h.events(sidD)) > 0 })
	if evs := h.events(sidD); evs[0].Binding != "unbound" {
		t.Fatalf("daemon hook bound: %+v", evs[0])
	}
	if _, ok := h.agentIn(pane); ok {
		t.Fatal("daemon pane listed as an agent")
	}
}

// watcher runs `tmag watch` and reads its JSON lines.
type watcher struct {
	t     *testing.T
	cmd   *exec.Cmd
	lines *bufio.Scanner
}

func (h *harness) watch(args ...string) *watcher {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(bin, "tmag"), append([]string{"watch"}, args...)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		_ = cmd.Process.Kill() // already stopped in the normal case
		_ = cmd.Wait()
	})
	return &watcher{t: h.t, cmd: cmd, lines: bufio.NewScanner(out)}
}

func (w *watcher) next() map[string]any {
	w.t.Helper()
	ch := make(chan map[string]any, 1)
	go func() {
		var doc map[string]any
		if w.lines.Scan() {
			if err := json.Unmarshal(w.lines.Bytes(), &doc); err != nil {
				doc = map[string]any{"invalid": w.lines.Text()}
			}
		}
		ch <- doc
	}()
	select {
	case doc := <-ch:
		return doc
	case <-time.After(5 * time.Second):
		w.t.Fatal("watch produced nothing")
	}
	return nil
}

// stop ends the watcher the way a disconnecting client would.
func (w *watcher) stop() {
	w.t.Helper()
	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		w.t.Fatal(err)
	}
	if err := w.cmd.Wait(); err != nil {
		w.t.Fatalf("watch exited with %v", err)
	}
}

func TestWatchResumes(t *testing.T) {
	h := start(t)
	w := h.watch()
	if doc := w.next(); doc["type"] != "snapshot" || doc["schema"] != "tmag/v1" {
		t.Fatalf("first line = %v", doc)
	}
	h.externalHook("claude", map[string]any{"hook_event_name": "Stop", "session_id": sidA, "cwd": h.dir})
	doc := w.next()
	ev, _ := doc["event"].(map[string]any)
	if doc["type"] != "event" || ev["session"] != sidA || ev["state"] != "idle" {
		t.Fatalf("event line = %v", doc)
	}
	w.stop()

	seq := int64(ev["seq"].(float64))
	h.externalHook("claude", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "cwd": h.dir, "prompt_id": "p2"})
	doc = h.watch("--since", fmt.Sprint(seq)).next()
	ev, _ = doc["event"].(map[string]any)
	if doc["type"] != "event" || ev["seq"].(float64) != float64(seq+1) || ev["state"] != "working" {
		t.Fatalf("resumed line = %v", doc)
	}

	// A cursor the log cannot vouch for restarts from a snapshot.
	if doc := h.watch("--since", "1").next(); doc["type"] != "snapshot" {
		t.Fatalf("stale cursor line = %v", doc)
	}
}

func TestWatchReportsAgentsThatChangeWithoutEvents(t *testing.T) {
	h := start(t)
	w := h.watch()
	agentsIn := func(doc map[string]any) []any {
		if doc["type"] != "snapshot" {
			t.Fatalf("expected a snapshot, got %v", doc)
		}
		list, _ := doc["agents"].([]any)
		return list
	}
	if n := len(agentsIn(w.next())); n != 0 {
		t.Fatalf("initial snapshot lists %d agents", n)
	}
	// An agent whose hooks never run: no event will ever mention it.
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	list := agentsIn(w.next())
	if len(list) != 1 || list[0].(map[string]any)["pane"].(map[string]any)["id"] != pane {
		t.Fatalf("new agent missing: %v", list)
	}
	h.tmux("kill-pane", "-t", pane)
	if list := agentsIn(w.next()); len(list) != 0 {
		t.Fatalf("closed pane still listed: %v", list)
	}
	w.stop()
}

func TestRestartedAgentDoesNotInheritRecord(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	claude := filepath.Join(bin, "claude")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out},
		"sh", "-c", claude+"; FAKE_HOOKS=0 FAKE_SESSION= "+claude)
	h.waitState(pane, "idle", "process")
	// Kill the first agent without SessionEnd; the shell starts a silent one.
	if err := exec.Command("pkill", "-KILL", "-o", "-f", "^"+claude+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	a := h.waitState(pane, "unknown", "unbound")
	if a.ID != "" {
		t.Fatalf("restarted agent shows the old session: %+v", a)
	}
	if r := h.tmag("", "send", sidA[:8], "hello"); r.code == 0 {
		t.Fatalf("send reached a replaced agent: %s", r.out)
	}
	if b, _ := os.ReadFile(out); len(b) != 0 {
		t.Fatalf("replaced agent received %q", b)
	}
}

func TestSendAndHooksStayOrdered(t *testing.T) {
	// The agent starts and ends its turn as soon as the prompt arrives, so its
	// hooks race the sender's own bookkeeping.
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "0"}, filepath.Join(bin, "claude"))
	h.waitState(pane, "idle", "process")
	for i := range 8 {
		r := h.tmag("", "send", sidA[:8], "--wait", "--json", "--timeout", "10s", fmt.Sprintf("prompt %d", i))
		var res struct {
			State   string `json:"state"`
			Started bool   `json:"started"`
			Turn    string `json:"turn"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.out), &res) != nil || !res.Started || res.State != "idle" || res.Turn == "" {
			t.Fatalf("send %d: %d %s", i, r.code, r.out)
		}
		// The finished turn leaves no pending prompt behind: the agent takes
		// the next one at once.
		if a, _ := h.agentIn(pane); a.State != "idle" || a.Kind != "" {
			t.Fatalf("after send %d the agent is %s (%s)", i, a.State, a.Kind)
		}
	}
	var order []string
	for _, e := range h.events(sidA) {
		if e.Event != "SessionStart" {
			order = append(order, e.Event)
		}
	}
	for i := 0; i+2 < len(order); i += 3 {
		if order[i] != "Send" || order[i+1] != "UserPromptSubmit" || order[i+2] != "Stop" {
			t.Fatalf("events out of order: %v", order)
		}
	}
	if len(order) != 3*8 {
		t.Fatalf("got %d events: %v", len(order), order)
	}
}

func TestHooksWaitForThePaneLock(t *testing.T) {
	// A sender holds the pane lock from delivery until its Send event is
	// logged; a hook arriving meanwhile must not update the record under it.
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	a := h.waitState(pane, "idle", "process")
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "prompt_id": "p1"})
	hook := exec.Command(filepath.Join(bin, "tmag"), "hook", "claude")
	hook.Stdin = bytes.NewReader(payload)
	err := registry.Lock(context.Background(), h.state, a.Pane.Server, pane, func() error {
		if err := hook.Start(); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		if a, _ := h.agentIn(pane); a.State != "idle" {
			return fmt.Errorf("hook updated a locked pane: %s", a.State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.Wait(); err != nil {
		t.Fatal(err)
	}
	if a := h.waitState(pane, "working", "session"); a.Turn != "p1" {
		t.Fatalf("turn = %q", a.Turn)
	}
}

func TestSendRefusesSuspendedAgent(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, "bash", "--norc", "--noprofile", "-i")
	h.tmux("send-keys", "-t", pane, filepath.Join(bin, "claude"), "Enter")
	h.waitState(pane, "idle", "process")
	// Ctrl-Z: the agent keeps its pid and record, but bash owns the terminal.
	h.tmux("send-keys", "-t", pane, "C-z")
	h.eventually("bash in the foreground", func() bool {
		a, ok := h.agentIn(pane)
		return ok && a.Pane.Command == "bash" && a.State == "idle"
	})
	marker := filepath.Join(h.dir, "ran-in-shell")
	r := h.tmag("", "send", sidA[:8], "touch "+marker)
	if r.code != 3 || !strings.Contains(r.out, "refused (background)") {
		t.Fatalf("send to a suspended agent: %d %s", r.code, r.out)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the prompt ran as a shell command")
	}
	// Back in the foreground, the same agent accepts the prompt.
	h.tmux("send-keys", "-t", pane, "fg", "Enter")
	h.eventually("claude in the foreground", func() bool {
		a, ok := h.agentIn(pane)
		return ok && a.Pane.Command == "claude"
	})
	if r := h.tmag("", "send", sidA[:8], "--wait", "hello"); r.code != 0 {
		t.Fatalf("send after fg: %d %s", r.code, r.out)
	}
}

func TestStaleRecordIsClearedForTmuxDisplays(t *testing.T) {
	// Pane borders and the picker read @tmag_* options straight from tmux,
	// checking only that the pane still runs a command of the recorded name.
	for trigger, clear := range map[string]func(h *harness){
		"status job": func(h *harness) {
			if r := h.tmag("", "status", "--tmux"); r.code != 0 {
				h.t.Fatalf("status: %d %s", r.code, r.out)
			}
		},
		"another agent's hook": func(h *harness) {
			h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"}, filepath.Join(bin, "codex"))
		},
	} {
		t.Run(trigger, func(t *testing.T) {
			h := start(t)
			claude := filepath.Join(bin, "claude")
			pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"},
				"sh", "-c", claude+"; FAKE_HOOKS=0 FAKE_SESSION= "+claude)
			h.waitState(pane, "idle", "process")
			h.tmux("send-keys", "-t", pane, "block", "Enter")
			h.waitState(pane, "blocked", "process")
			// Killed while blocked, without SessionEnd; a silent claude follows.
			if err := exec.Command("pkill", "-KILL", "-o", "-f", "^"+claude+"$").Run(); err != nil {
				t.Fatalf("pkill: %v", err)
			}
			h.waitState(pane, "unknown", "unbound")
			display := "#{?#{==:#{pane_current_command},#{@tmag_cmd}},#{@tmag_state},}"
			if got := h.tmux("display-message", "-p", "-t", pane, display); got != "blocked" {
				t.Fatalf("precondition: tmux shows %q", got)
			}
			clear(h)
			h.eventually("the stale record to be cleared", func() bool {
				return h.tmux("display-message", "-p", "-t", pane, display) == ""
			})
		})
	}
}

func TestSendCannotEscapeBracketedPaste(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out}, filepath.Join(bin, "claude"))
	h.waitState(pane, "idle", "process")
	payload := "abc\x1b[201~INJECT\rmore\x07"
	if r := h.tmag(payload, "send", h.target(pane), "--wait"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitState(pane, "idle", "process")
	got, _ := os.ReadFile(out)
	if strings.ContainsRune(string(got), 0x1b) || strings.ContainsRune(string(got), 0x07) {
		t.Fatalf("control characters reached the agent: %q", got)
	}
	if !strings.Contains(string(got), "abc[201~INJECT") {
		t.Fatalf("agent received %q", got)
	}
}

func TestPaneFieldsCannotBeForged(t *testing.T) {
	h := start(t)
	// A directory name carrying the field separator and forged option values.
	evil := filepath.Join(h.dir, "x\x1f"+sidB+"\x1fidle\x1fprocess")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	pane := h.spawn(evil, map[string]string{}, filepath.Join(bin, "codex"))
	a := h.waitState(pane, "unknown", "unbound")
	if a.ID != "" || strings.ContainsRune(a.Pane.Cwd, 0x1f) {
		t.Fatalf("forged fields: %+v", a)
	}
}

func TestSymlinkedSocketDirListsEachServerOnce(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitState(pane, "idle", "process")
	link := filepath.Join(h.dir, "tmpdir-link")
	if err := os.Symlink(os.Getenv("TMUX_TMPDIR"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", link)
	// Run inside the pane's server too, as a user would.
	sock := strings.Split(h.tmux("display-message", "-p", "#{socket_path}"), "\n")[0]
	t.Setenv("TMUX", filepath.Join(link, filepath.Base(filepath.Dir(sock)), filepath.Base(sock))+",1,0")
	defer os.Unsetenv("TMUX")
	agents := h.agents()
	if len(agents) != 1 {
		t.Fatalf("listed %d agents: %+v", len(agents), agents)
	}
	if r := h.tmag("", "send", sidA[:8], "hi"); r.code != 0 {
		t.Fatalf("send by id: %d %s", r.code, r.out)
	}
}

func TestWorksWithoutUTF8Locale(t *testing.T) {
	h := start(t)
	// Hooks and daemons often run without a UTF-8 locale; tmux then mangles
	// output unless tmag asks for UTF-8 itself.
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TITLE": "✳ Claude Code"}, filepath.Join(bin, "claude"))
	a := h.waitState(pane, "idle", "process")
	if a.Pane.Title != "✳ Claude Code" {
		t.Fatalf("title = %q", a.Pane.Title)
	}
}
