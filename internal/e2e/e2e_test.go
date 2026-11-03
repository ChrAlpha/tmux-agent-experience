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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
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

// view is `tmag list --json`.
func (h *harness) view() query.View {
	h.t.Helper()
	r := h.tmag("", "list", "--json")
	var doc struct {
		Schema string `json:"schema"`
		query.View
	}
	if err := json.Unmarshal([]byte(strings.SplitN(r.out, "\n", 2)[0]), &doc); err != nil || doc.Schema != "tmag/v1" {
		h.t.Fatalf("list --json: %v %q", err, r.out)
	}
	return doc.View
}

// paneIn returns the agent pane and the session it shows, if any.
func (h *harness) paneIn(pane string) (query.Pane, query.Session, bool) {
	v := h.view()
	for _, p := range v.Panes {
		if p.ID == pane {
			s, _ := v.Session(p.Agent, p.Session)
			return p, s, true
		}
	}
	return query.Pane{}, query.Session{}, false
}

func (h *harness) sessionOf(id string) (query.Session, bool) {
	for _, s := range h.view().Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return query.Session{}, false
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
	h.t.Fatalf("timed out waiting for %s; view: %+v", what, h.view())
}

// waitPane waits until the pane shows a session in state, by evidence; an
// empty evidence waits until it shows none.
func (h *harness) waitPane(pane, state, evidence string) (query.Pane, query.Session) {
	h.t.Helper()
	var gp query.Pane
	var gs query.Session
	h.eventually(fmt.Sprintf("%s to show a %s session by %q", pane, state, evidence), func() bool {
		p, s, ok := h.paneIn(pane)
		gp, gs = p, s
		if !ok || p.Evidence != evidence {
			return false
		}
		return evidence == "" || s.State == state
	})
	return gp, gs
}

func (h *harness) waitSession(id, state string) query.Session {
	h.t.Helper()
	var got query.Session
	h.eventually(fmt.Sprintf("session %s to be %s", id, state), func() bool {
		s, ok := h.sessionOf(id)
		got = s
		return ok && s.State == state
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

// eventNames lists a session's events as event:state.
func (h *harness) eventNames(session string) string {
	var names []string
	for _, e := range h.events(session) {
		names = append(names, e.Event+":"+e.State)
	}
	return strings.Join(names, " ")
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
	p, s := h.waitPane(pane, "idle", "process")
	if s.ID != sidA || s.Agent != "claude" || s.Runtime != "terminal" || p.Command != "claude" || p.View != "main" {
		t.Fatalf("pane %+v shows %+v", p, s)
	}
	if !slices.Equal(s.Capabilities, []string{"send"}) {
		t.Fatalf("capabilities = %v", s.Capabilities)
	}
	transcript := eventlog.Transcript{Path: filepath.Join(h.dir, sidA+".jsonl"), Format: "claude-jsonl"}
	if s.Transcript == nil || *s.Transcript != transcript {
		t.Fatalf("transcript = %+v", s.Transcript)
	}
	if ev := h.events(sidA)[0]; ev.Runtime != "terminal" || ev.Transcript == nil || *ev.Transcript != transcript || ev.Cwd != h.dir {
		t.Fatalf("first event does not report the session: %+v", ev)
	}

	r := h.tmag("", "send", sidA[:8], "--wait", "--json", "review", "the", "diff")
	if r.code != 0 || !strings.Contains(r.out, `"state":"idle"`) {
		t.Fatalf("send --wait: %d %s", r.code, r.out)
	}
	if got, _ := os.ReadFile(out); string(got) != "review the diff\n" {
		t.Fatalf("agent received %q", got)
	}
	if got, want := h.eventNames(sidA), "SessionStart:idle Attach:idle Send: UserPromptSubmit:working Stop:idle"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}

	// A second prompt while the first is running is refused, not queued.
	if r := h.tmag("first", "send", h.target(pane)); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "send", h.target(pane), "second"); r.code != 3 || !strings.Contains(r.out, "busy") {
		t.Fatalf("busy send: %d %s", r.code, r.out)
	}
	h.waitPane(pane, "idle", "process")
	if r := h.tmag("", "wait", h.target(pane), "--json"); r.code != 0 || !strings.Contains(r.out, `"idle"`) {
		t.Fatalf("wait on idle agent: %d %s", r.code, r.out)
	}

	// A prompt that starts no turn (a slash command) releases the agent.
	r = h.tmag("", "send", h.target(pane), "--wait", "--json", "/model")
	if r.code != 0 || !strings.Contains(r.out, `"started":false`) {
		t.Fatalf("send slash command --wait: %d %s", r.code, r.out)
	}

	// A waiter learns that the session ended instead of timing out.
	waiter := exec.Command(filepath.Join(bin, "tmag"), "wait", sidA[:8], "--for", "blocked", "--timeout", "20s", "--json")
	var wout bytes.Buffer
	waiter.Stdout = &wout
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Ending the session detaches its pane.
	h.tmux("send-keys", "-t", pane, "C-d")
	h.eventually("session end", func() bool { return strings.HasSuffix(h.eventNames(sidA), "SessionEnd: Detach:") })
	err := waiter.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(wout.String(), `"ended"`) {
		t.Fatalf("waiter: %v %s", err, wout.String())
	}
	if _, ok := h.sessionOf(sidA); ok {
		t.Fatal("ended session still listed")
	}
}

// A Claude Code background session runs in its supervisor, which no
// terminal owns: it is tracked, and refuses what only a terminal can carry.
func TestClaudeBackgroundSessionTakesNoInput(t *testing.T) {
	h := start(t)
	worker := exec.Command(filepath.Join(bin, "claude"), "daemon", "run")
	worker.Env = append(os.Environ(), "FAKE_SESSION="+sidA, "FAKE_HOOKS=1", "TMAG_BIN="+filepath.Join(bin, "tmag"))
	in, err := worker.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		_ = worker.Wait()
	})
	s := h.waitSession(sidA, "idle")
	if s.Runtime != "service" || len(s.Panes) != 0 || len(s.Capabilities) != 0 {
		t.Fatalf("background session = %+v", s)
	}
	if r := h.tmag("", "send", sidA[:8], "hello"); r.code != 3 || !strings.Contains(r.out, "unsupported") {
		t.Fatalf("send to a background session: %d %s", r.code, r.out)
	}
}

func TestPaneWithoutProofIsVisibleButNotControllable(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	p, _ := h.waitPane(pane, "", "")
	if p.Session != "" || p.Agent != "claude" {
		t.Fatalf("pane without hooks = %+v", p)
	}
	if r := h.tmag("", "send", h.target(pane), "hello"); r.code != 3 || !strings.Contains(r.out, "unbound") {
		t.Fatalf("send to a pane that shows no session: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "wait", h.target(pane)); r.code != 3 || !strings.Contains(r.out, "unbound") {
		t.Fatalf("wait on a pane that shows no session: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "read", h.target(pane), "--lines", "5"); r.code != 0 || !strings.Contains(r.out, "ready") {
		t.Fatalf("read: %d %q", r.code, r.out)
	}
}

// fakeDaemon is a Codex app-server daemon outside every pane, running hooks
// for any thread.
type fakeDaemon struct {
	h     *harness
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines *bufio.Scanner
}

func (h *harness) daemon() *fakeDaemon {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(bin, "codex"), "app-server", "--listen", "unix://", "--managed-daemon")
	cmd.Env = append(os.Environ(), "TMAG_BIN="+filepath.Join(bin, "tmag"))
	in, err := cmd.StdinPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		_ = cmd.Process.Kill() // may be killed by the test already
		_ = cmd.Wait()
	})
	return &fakeDaemon{h: h, cmd: cmd, in: in, lines: bufio.NewScanner(out)}
}

// hook runs one hook in the daemon and waits for it to finish.
func (d *fakeDaemon) hook(payload map[string]any) {
	d.h.t.Helper()
	b, _ := json.Marshal(payload)
	if _, err := d.in.Write(append(b, '\n')); err != nil {
		d.h.t.Fatal(err)
	}
	if !d.lines.Scan() {
		d.h.t.Fatal("daemon stopped")
	}
}

// codexTitle renders a Codex 0.166 title with the thread-id item last.
func codexTitle(name, id string) string { return name + " | repo | " + id[:29] + "..." }

func TestCodexDaemonSessions(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	d := h.daemon()
	byID := h.spawn(repo, map[string]string{"FAKE_TITLE": codexTitle("⠋ Refactor parser", sidB)}, filepath.Join(bin, "codex"))
	// A name alone proves nothing, even one that only this thread carries.
	named := h.spawn(repo, map[string]string{"FAKE_TITLE": "Refactor parser | repo"}, filepath.Join(bin, "codex"))
	h.waitPane(named, "", "")

	// A session is tracked from its first hook, whether or not a pane shows it.
	d.hook(map[string]any{"hook_event_name": "PermissionRequest", "session_id": sidC, "cwd": repo, "turn_id": "t1", "tool_name": "Bash"})
	if s := h.waitSession(sidC, "blocked"); s.Kind != "permission" || s.Runtime != "service" || len(s.Panes) != 0 {
		t.Fatalf("daemon session = %+v", s)
	}
	if r := h.tmag("", "list"); !strings.Contains(r.out, "/title") {
		t.Fatalf("list does not explain how to show Codex threads:\n%s", r.out)
	}

	// The thread-id title item attaches the pane, before or after the hook.
	d.hook(map[string]any{"hook_event_name": "Stop", "session_id": sidB, "cwd": repo, "turn_id": "t1"})
	if _, s := h.waitPane(byID, "idle", "title-id"); s.ID != sidB || s.Turn != "t1" {
		t.Fatalf("title-id pane shows %+v", s)
	}
	if p, _, _ := h.paneIn(named); p.Session != "" {
		t.Fatalf("named pane attached to %s", p.Session)
	}
	if r := h.tmag("", "send", h.target(byID), "--json", "hello"); r.code != 0 {
		t.Fatalf("send through title-id: %d %s", r.code, r.out)
	}

	// Watching a subagent: still the session's pane, but it takes no input.
	sub := "0f000000-0000-7000-8000-00000000000f"
	d.hook(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidB, "cwd": repo, "turn_id": "t2"})
	d.hook(map[string]any{"hook_event_name": "Stop", "session_id": sidB, "cwd": repo, "turn_id": "t2"})
	d.hook(map[string]any{"hook_event_name": "SubagentStart", "session_id": sidB, "cwd": repo, "turn_id": "t9", "agent_id": sub, "agent_type": "default"})
	if s := h.waitSession(sidB, "working"); s.Kind != "background" || s.Turn != "t2" {
		t.Fatalf("a running subagent after Stop: %+v", s)
	}
	d.hook(map[string]any{"hook_event_name": "SubagentStop", "session_id": sidB, "cwd": repo, "turn_id": "t9", "agent_id": sub})
	h.waitSession(sidB, "idle")
	h.tmux("select-pane", "-t", byID, "-T", codexTitle("sub", sub))
	if p, _ := h.waitPane(byID, "idle", "title-id"); p.View != "subagent" || p.Session != sidB {
		t.Fatalf("subagent view = %+v", p)
	}
	if r := h.tmag("", "send", h.target(byID), "hello"); r.code != 3 || !strings.Contains(r.out, "subagent") {
		t.Fatalf("send to a subagent view: %d %s", r.code, r.out)
	}

	// Showing another thread moves the pane to it.
	h.tmux("select-pane", "-t", byID, "-T", codexTitle("other", sidC))
	h.tmag("", "status") // any reconciliation records the move
	if _, s := h.waitPane(byID, "blocked", "title-id"); s.ID != sidC {
		t.Fatalf("pane shows %s after switching threads", s.ID)
	}
	if !strings.HasSuffix(h.eventNames(sidB), "Detach:") || !strings.Contains(h.eventNames(sidC), "Attach:blocked") {
		t.Fatalf("switch events: %s / %s", h.eventNames(sidB), h.eventNames(sidC))
	}

	// The daemon's threads end with it, SessionEnd or not.
	if err := d.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = d.cmd.Wait()
	h.tmag("", "status")
	for _, id := range []string{sidB, sidC} {
		if got := h.eventNames(id); !strings.Contains(got, "Exit:") {
			t.Fatalf("%s events after the daemon exited: %s", id, got)
		}
	}
	if v := h.view(); len(v.Sessions) != 0 {
		t.Fatalf("sessions outlived their daemon: %+v", v.Sessions)
	}
}

func TestDaemonInsidePaneDoesNotAttach(t *testing.T) {
	h := start(t)
	// A Codex daemon started from a pane runs hooks for every thread; its
	// pane ancestry must not attribute them to that pane.
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidD, "FAKE_HOOKS": "1"}, filepath.Join(bin, "codex"), "app-server", "--managed-daemon")
	s := h.waitSession(sidD, "idle")
	if len(s.Panes) != 0 || s.Runtime != "service" {
		t.Fatalf("daemon session = %+v", s)
	}
	if _, _, ok := h.paneIn(pane); ok {
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

// until reads lines until one satisfies ok, and returns it.
func (w *watcher) until(what string, ok func(doc map[string]any) bool) map[string]any {
	w.t.Helper()
	for range 20 {
		if doc := w.next(); ok(doc) {
			return doc
		}
	}
	w.t.Fatalf("watch never reported %s", what)
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

func eventOf(doc map[string]any) map[string]any {
	ev, _ := doc["event"].(map[string]any)
	return ev
}

func TestWatchResumes(t *testing.T) {
	h := start(t)
	w := h.watch()
	if doc := w.next(); doc["type"] != "snapshot" || doc["schema"] != "tmag/v1" {
		t.Fatalf("first line = %v", doc)
	}
	h.externalHook("claude", map[string]any{"hook_event_name": "Stop", "session_id": sidA, "cwd": h.dir})
	doc := w.next()
	ev := eventOf(doc)
	if doc["type"] != "event" || ev["session"] != sidA || ev["state"] != "idle" {
		t.Fatalf("event line = %v", doc)
	}
	w.stop()

	seq := int64(ev["seq"].(float64))
	h.externalHook("claude", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "cwd": h.dir, "prompt_id": "p2"})
	resumed := h.watch("--since", fmt.Sprint(seq))
	doc = resumed.next()
	ev = eventOf(doc)
	if doc["type"] != "event" || ev["seq"].(float64) != float64(seq+1) || ev["state"] != "working" {
		t.Fatalf("resumed line = %v", doc)
	}
	if doc := resumed.next(); doc["type"] != "panes" {
		t.Fatalf("resumed watch did not send the pane list: %v", doc)
	}

	// A cursor the log cannot vouch for restarts from a snapshot.
	if doc := h.watch("--since", "1").next(); doc["type"] != "snapshot" {
		t.Fatalf("stale cursor line = %v", doc)
	}
}

func TestWatchReportsWhatChangesWithoutHooks(t *testing.T) {
	h := start(t)
	w := h.watch()
	if doc := w.next(); doc["type"] != "snapshot" || len(doc["panes"].([]any)) != 0 {
		t.Fatalf("initial snapshot = %v", doc)
	}
	// An agent whose hooks never run: only the pane list mentions it.
	quiet := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	doc := w.until("the new pane", func(doc map[string]any) bool { return doc["type"] == "panes" })
	if list := doc["panes"].([]any); len(list) != 1 || list[0].(map[string]any)["id"] != quiet {
		t.Fatalf("pane list = %v", list)
	}
	h.tmux("kill-pane", "-t", quiet)
	doc = w.until("the closed pane", func(doc map[string]any) bool { return doc["type"] == "panes" })
	if list := doc["panes"].([]any); len(list) != 0 {
		t.Fatalf("closed pane still listed: %v", list)
	}

	// An agent killed without SessionEnd: its pane detaches and its session
	// exits, as events.
	claude := filepath.Join(bin, "claude")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"}, "sh", "-c", claude+"; sleep 60")
	h.waitPane(pane, "idle", "process")
	if err := exec.Command("pkill", "-KILL", "-f", "^"+claude+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	seen := map[string]bool{}
	w.until("detach and exit", func(doc map[string]any) bool {
		if ev := eventOf(doc); ev != nil && ev["session"] == sidB {
			seen[ev["event"].(string)] = true
		}
		return seen["Detach"] && seen["Exit"]
	})
	w.stop()
}

func TestRestartedAgentDoesNotInheritTheSession(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	claude := filepath.Join(bin, "claude")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out},
		"sh", "-c", claude+"; FAKE_HOOKS=0 FAKE_SESSION= "+claude)
	h.waitPane(pane, "idle", "process")
	// Kill the first agent without SessionEnd; the shell starts a silent one.
	if err := exec.Command("pkill", "-KILL", "-o", "-f", "^"+claude+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	h.waitPane(pane, "", "")
	if r := h.tmag("", "send", sidA[:8], "hello"); r.code == 0 {
		t.Fatalf("send reached a replaced agent: %s", r.out)
	}
	if r := h.tmag("", "send", h.target(pane), "hello"); r.code != 3 {
		t.Fatalf("send to the replacement: %d %s", r.code, r.out)
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
	h.waitPane(pane, "idle", "process")
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
		if s, _ := h.sessionOf(sidA); s.State != "idle" || s.Kind != "" {
			t.Fatalf("after send %d the session is %s (%s)", i, s.State, s.Kind)
		}
	}
	var order []string
	for _, e := range h.events(sidA) {
		switch e.Event {
		case "Send", "UserPromptSubmit", "Stop":
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

func TestHooksWaitForTheSessionLock(t *testing.T) {
	// A sender holds the session lock from delivery until its Send event is
	// logged; a hook arriving meanwhile must not update the session under it.
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sidA, "prompt_id": "p1"})
	hook := exec.Command(filepath.Join(bin, "tmag"), "hook", "claude")
	hook.Stdin = bytes.NewReader(payload)
	err := session.Store{Dir: h.state}.Lock(context.Background(), "claude", sidA, func() error {
		if err := hook.Start(); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		if s, _ := h.sessionOf(sidA); s.State != "idle" {
			return fmt.Errorf("hook updated a locked session: %s", s.State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.Wait(); err != nil {
		t.Fatal(err)
	}
	if s := h.waitSession(sidA, "working"); s.Turn != "p1" {
		t.Fatalf("turn = %q", s.Turn)
	}
}

func TestSendRefusesSuspendedAgent(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, "bash", "--norc", "--noprofile", "-i")
	h.tmux("send-keys", "-t", pane, filepath.Join(bin, "claude"), "Enter")
	h.waitPane(pane, "idle", "process")
	// Ctrl-Z: the agent keeps its pid and attachment, but bash owns the terminal.
	h.tmux("send-keys", "-t", pane, "C-z")
	h.eventually("bash in the foreground", func() bool {
		p, s, ok := h.paneIn(pane)
		return ok && p.Command == "bash" && s.State == "idle"
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
		p, _, ok := h.paneIn(pane)
		return ok && p.Command == "claude"
	})
	if r := h.tmag("", "send", sidA[:8], "--wait", "hello"); r.code != 0 {
		t.Fatalf("send after fg: %d %s", r.code, r.out)
	}
}

func TestStaleAttachmentIsClearedForTmuxDisplays(t *testing.T) {
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
			h.waitPane(pane, "idle", "process")
			h.tmux("send-keys", "-t", pane, "block", "Enter")
			h.waitPane(pane, "blocked", "process")
			// Killed while blocked, without SessionEnd; a silent claude follows.
			if err := exec.Command("pkill", "-KILL", "-o", "-f", "^"+claude+"$").Run(); err != nil {
				t.Fatalf("pkill: %v", err)
			}
			h.waitPane(pane, "", "")
			display := "#{?#{==:#{pane_current_command},#{@tmag_cmd}},#{@tmag_state},}"
			if got := h.tmux("display-message", "-p", "-t", pane, display); got != "blocked" {
				t.Fatalf("precondition: tmux shows %q", got)
			}
			clear(h)
			h.eventually("the stale attachment to be cleared", func() bool {
				return h.tmux("display-message", "-p", "-t", pane, display) == ""
			})
		})
	}
}

func TestSendCannotEscapeBracketedPaste(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_OUT": out}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	payload := "abc\x1b[201~INJECT\rmore\x07"
	if r := h.tmag(payload, "send", h.target(pane), "--wait"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
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
	// A directory name carrying field separators and forged option values.
	evil := filepath.Join(h.dir, "x\t"+sidB+"\tidle\x1fprocess\t")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	pane := h.spawn(evil, map[string]string{}, filepath.Join(bin, "codex"))
	p, _ := h.waitPane(pane, "", "")
	if p.Session != "" || strings.ContainsAny(p.Cwd, "\t\x1f") {
		t.Fatalf("forged fields: %+v", p)
	}
}

func TestSymlinkedSocketDirListsEachServerOnce(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	link := filepath.Join(h.dir, "tmpdir-link")
	if err := os.Symlink(os.Getenv("TMUX_TMPDIR"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", link)
	// Run inside the pane's server too, as a user would.
	sock := strings.Split(h.tmux("display-message", "-p", "#{socket_path}"), "\n")[0]
	t.Setenv("TMUX", filepath.Join(link, filepath.Base(filepath.Dir(sock)), filepath.Base(sock))+",1,0")
	defer os.Unsetenv("TMUX")
	if v := h.view(); len(v.Panes) != 1 || len(v.Sessions) != 1 || len(v.Sessions[0].Panes) != 1 {
		t.Fatalf("listed %+v", v)
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
	dir := filepath.Join(h.dir, "✳ 项目")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pane := h.spawn(dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	want, err := filepath.EvalSymlinks(dir) // macOS: /var is /private/var
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := h.waitPane(pane, "idle", "process"); p.Cwd != want {
		t.Fatalf("cwd = %q", p.Cwd)
	}
}

func TestAttachNeedsItsClientStillRunning(t *testing.T) {
	// A hook proved the pane's agent, then the agent was replaced before the
	// hook took the pane's lock: its proof no longer stands.
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "", "")
	ctx := context.Background()
	env := attach.Env{Log: eventlog.Open(h.state), Store: session.Store{Dir: h.state}}
	rec := session.Record{Agent: "claude", ID: sidA, Main: "idle"}
	if err := env.Store.Put(rec); err != nil {
		t.Fatal(err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var client attach.Client
	for _, c := range w.Clients() {
		if c.Pane.ID == pane {
			client = c
		}
	}
	gone := client
	gone.Process.Start += "0" // the same pid, another process
	if err := env.Attach(ctx, w, gone, rec, attach.Process); err != nil {
		t.Fatal(err)
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_session}"); got != "" {
		t.Fatalf("attached for a process that is gone: %q", got)
	}
	if err := env.Attach(ctx, w, client, rec, attach.Process); err != nil {
		t.Fatal(err)
	}
	if p, s := h.waitPane(pane, "idle", "process"); s.ID != sidA || p.Session != sidA {
		t.Fatalf("pane %+v shows %+v", p, s)
	}
}

func TestAttachmentEventsSurviveACrash(t *testing.T) {
	// A writer died after changing a pane's attachment and before logging
	// it; the next reconciliation logs what the pane is marked with.
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	count := func(event string) int {
		n := 0
		for _, e := range h.events(sidA) {
			if e.Event == event {
				n++
			}
		}
		return n
	}
	h.tmux("set-option", "-p", "-t", pane, "@tmag_change", "Attach")
	h.tmag("", "status")
	if n := count("Attach"); n != 2 || h.tmux("display-message", "-p", "-t", pane, "#{@tmag_change}") != "" {
		t.Fatalf("pending Attach: %d Attach events, marker %q", n, h.tmux("display-message", "-p", "-t", pane, "#{@tmag_change}"))
	}
	h.tmux("set-option", "-p", "-t", pane, "@tmag_change", "Detach")
	if p, _, _ := h.paneIn(pane); p.Session != "" {
		t.Fatalf("a pane being detached still shows %s", p.Session)
	}
	h.tmag("", "status")
	if n := count("Detach"); n != 1 || h.tmux("display-message", "-p", "-t", pane, "#{@tmag_session}") != "" {
		t.Fatalf("pending Detach: %d Detach events", n)
	}
	// The agent's next hook proves the pane again.
	h.tmux("send-keys", "-t", pane, "hello", "Enter")
	h.waitPane(pane, "idle", "process")
	if n := count("Attach"); n != 3 {
		t.Fatalf("reattached with %d Attach events", n)
	}

	// Died while replacing another session's attachment: both events follow.
	h.tmux("set-option", "-p", "-t", pane, "@tmag_change", "Attach")
	h.tmux("set-option", "-p", "-t", pane, "@tmag_replaced", "claude "+sidB)
	h.tmag("", "status")
	if got := h.eventNames(sidB); got != "Detach:" || count("Attach") != 4 {
		t.Fatalf("replaced: %q, %d Attach events", got, count("Attach"))
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_change}#{@tmag_replaced}"); got != "" {
		t.Fatalf("markers left: %q", got)
	}

	// Options that hold no attachment are cleared rather than displayed.
	quiet := h.spawn(h.dir, map[string]string{}, filepath.Join(bin, "codex"))
	h.tmux("set-option", "-p", "-t", quiet, "@tmag_state", "blocked")
	h.tmag("", "status")
	if got := h.tmux("display-message", "-p", "-t", quiet, "#{@tmag_state}"); got != "" {
		t.Fatalf("broken options left: %q", got)
	}
}
