package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func hasHint(rec session.Record, pane string) bool {
	return rec.Pane != nil && rec.Pane.ID == pane && rec.Pane.Start != ""
}

// A hinted session's state hooks start no tmux process when the projection
// stays the same, and exactly one when it changes.
func TestHintedHookUpdatesOnePaneWithOneTmuxProcess(t *testing.T) {
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wrap := filepath.Join(dir, "bin")
	if err := os.MkdirAll(wrap, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "tmux-invocations")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$(ps -o args= -p \"$PPID\")\" \"$*\" >> '" +
		strings.ReplaceAll(log, "'", "'\\''") + "'\nexec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := start(t)
	// Push the agent pane past %9 so a literal pane id in a display template
	// would be strftime-damaged.
	for range 10 {
		h.tmux("new-window", "-d", "/bin/sh", "-c", "sleep 600")
	}
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(log)
			t.Logf("tmux invocations: %s", b)
			rec, ok, err := (session.Store{Dir: h.state}).Get(context.Background(), proc.Claude, sidA)
			t.Logf("record: %+v ok=%v err=%v", rec, ok, err)
		}
	})
	claude := filepath.Join(bin, "claude")
	pane := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_HOOKS": "1",
	}, claude)
	h.waitPane(pane, "idle", "process")
	store := session.Store{Dir: h.state}
	h.eventually("a pane hint", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && hasHint(rec, pane)
	})
	// SessionStart publishes the hint before it finishes; wait for it to exit
	// so its remaining reads are not counted with the first prompt.
	h.waitHookExit(claude)
	hookInvocations := func() int {
		b, _ := os.ReadFile(log)
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "tmag") && strings.Contains(line, "hook") {
				n++
			}
		}
		return n
	}
	clear := func() { _ = os.Remove(log) }
	projection := func() string {
		return h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_kind}")
	}

	// UserPromptSubmit: idle to working is a projection change.
	clear()
	h.tmux("send-keys", "-t", pane, "sleep", "Enter")
	h.eventually("working after the prompt", func() bool { return projection() == "working|" })
	h.waitHookExit(claude)
	if got := hookInvocations(); got != 1 {
		t.Fatalf("prompt started %d tmux processes", got)
	}
	// The turn ends through /end, so its Stop is measured on its own.
	clear()
	h.tmux("send-keys", "-t", pane, "/end", "Enter")
	h.eventually("idle projection after the turn", func() bool { return projection() == "idle|" })
	h.waitHookExit(claude)
	if got := hookInvocations(); got != 1 {
		t.Fatalf("Stop started %d tmux processes", got)
	}

	// With a notifier enabled, the same one-process budget holds.
	h.tmux("set-option", "-g", "@tmag-notify", "true")
	h.tmux("set-option", "-g", "@tmag-alert", "finished,blocked")
	clear()
	h.tmux("send-keys", "-t", pane, "sleep", "Enter")
	h.eventually("working with a notifier", func() bool { return projection() == "working|" })
	h.waitHookExit(claude)
	clear()
	h.tmux("send-keys", "-t", pane, "/end", "Enter")
	h.eventually("idle projection with a notifier", func() bool { return projection() == "idle|" })
	h.waitHookExit(claude)
	if got := hookInvocations(); got != 1 {
		t.Fatalf("one-server Stop with notifier: got %d tmux processes, want 1", got)
	}

	// A finishing hook reuses the hinted server's options and reads each
	// other server once: three servers cost three tmux processes, not four.
	for _, name := range []string{"e2e-extra-one", "e2e-extra-two"} {
		cmd := exec.Command("tmux", "-L", name, "-f", "/dev/null", "new-session", "-d", "-s", "extra", "/bin/sh")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("extra server %s: %v %s", name, err, out)
		}
		name := name
		t.Cleanup(func() { _ = exec.Command("tmux", "-L", name, "kill-server").Run() })
	}
	clear()
	h.tmux("send-keys", "-t", pane, "sleep", "Enter")
	h.eventually("working on three servers", func() bool { return projection() == "working|" })
	h.waitHookExit(claude)
	clear()
	h.tmux("send-keys", "-t", pane, "/end", "Enter")
	h.eventually("idle projection on three servers", func() bool { return projection() == "idle|" })
	h.waitHookExit(claude)
	if got := hookInvocations(); got != 3 {
		t.Fatalf("three-server Stop: got %d tmux processes, want 3", got)
	}

	// The first PreToolUse changes idle to working; the second changes no
	// projection at all.
	clear()
	h.tmux("send-keys", "-t", pane, "/pretool", "Enter")
	h.eventually("working after the tool", func() bool { return projection() == "working|" })
	h.waitHookExit(claude)
	if got := hookInvocations(); got != 1 {
		t.Fatalf("the first tool started %d tmux processes", got)
	}
	clear()
	h.tmux("send-keys", "-t", pane, "/pretool", "Enter")
	h.eventually("the second call recorded", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && len(rec.Calls) >= 2
	})
	h.waitHookExit(claude)
	if got := hookInvocations(); got != 0 {
		t.Fatalf("no-op tool started %d tmux processes", got)
	}
	if got := projection(); got != "working|" {
		t.Fatalf("projection after the tool = %q", got)
	}
}

// The guard writes nothing when the pane's attachment changed under the hint;
// the hook clears it and takes the full proof path, which restores the pane.
func TestHintGuardRefusesAChangedAttachment(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "100"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	store := session.Store{Dir: h.state}
	h.eventually("a pane hint", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && hasHint(rec, pane)
	})
	h.tmux("set-option", "-p", "-t", pane, "@tmag_session", sidB)
	h.tmux("send-keys", "-t", pane, "/pretool", "Enter")
	h.eventually("the full path restored the attachment", func() bool {
		return h.tmux("display-message", "-p", "-t", pane, "#{@tmag_session}|#{@tmag_state}|#{@tmag_kind}") == sidA+"|working|"
	})
	// The Attach writes the pane before it saves the hint.
	h.eventually("the full path restored the hint", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && hasHint(rec, pane)
	})
}

// A session shown in two panes is projected onto both by a fast hook, without
// waiting for a reconciling read.
func TestSharedSessionProjection(t *testing.T) {
	h := start(t)
	// High pane ids also exercise the sibling list's #{pane_id} output.
	for range 10 {
		h.tmux("new-window", "-d", "/bin/sh", "-c", "sleep 600")
	}
	spawn := func() string {
		return h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	}
	first := spawn()
	ready := func(p string) {
		h.eventually("agent ready", func() bool {
			return strings.Contains(h.tmux("capture-pane", "-p", "-t", p), "ready")
		})
	}
	ready(first)
	second := spawn()
	ready(second)
	state := func(p string) string {
		return h.tmux("display-message", "-p", "-t", p, "#{@tmag_state}")
	}
	if state(first) != "idle" || state(second) != "idle" {
		t.Fatalf("initial states: %q, %q", state(first), state(second))
	}
	h.tmux("send-keys", "-t", second, "block", "Enter")
	h.eventually("source blocked", func() bool { return state(second) == "blocked" })
	h.eventually("all attached panes blocked", func() bool { return state(first) == "blocked" })
}

// A sibling pane whose proven client died is not projected or belled by the
// full path: its options still name the session, but Check refuses it.
func TestDeadSiblingClientIsNotProjected(t *testing.T) {
	h := start(t)
	claude := filepath.Join(bin, "claude")
	spawn := func(shell bool) string {
		args := []string{claude}
		if shell {
			args = []string{"sh", "-c", claude + "; exec sleep 600"}
		}
		return h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, args...)
	}
	ready := func(p string) {
		h.eventually("agent ready", func() bool {
			return strings.Contains(h.tmux("capture-pane", "-p", "-t", p), "ready")
		})
	}
	dead := spawn(true)
	ready(dead)
	source := spawn(false)
	ready(source)
	pid, err := strconv.Atoi(h.tmux("display-message", "-p", "-t", dead, "#{@tmag_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.eventually("the sibling client exited", func() bool {
		return h.tmux("display-message", "-p", "-t", dead, "#{pane_current_command}") == "sleep"
	})
	h.tmux("send-keys", "-t", source, "block", "Enter")
	h.eventually("source blocked", func() bool {
		return h.tmux("display-message", "-p", "-t", source, "#{@tmag_state}") == "blocked"
	})
	// The source's hook may still project the panes it lists; wait until it is done.
	h.waitHookExit(claude)
	if got := h.tmux("display-message", "-p", "-t", dead, "#{@tmag_state}"); got == "blocked" {
		t.Fatal("a dead client's pane was projected")
	}
}

// Attach writes a pane's old projection, then reads the record again; a hook
// that saved a newer state meanwhile is picked up by that second projection.
func TestAttachPicksUpAChangedRecord(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := session.Store{Dir: h.state}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var c attach.Client
	for _, p := range w.Panes {
		if p.ID == pane {
			c, _ = w.ClientIn(p)
		}
	}
	if c.Process.PID == 0 {
		t.Fatal("no client in the pane")
	}
	sock := h.tmux("display-message", "-p", "#{socket_path}")
	hint := session.PaneHint{Server: sock, ID: pane, PID: c.Process.PID, Start: c.Process.Start}
	rec, ok, err := store.Get(ctx, proc.Claude, sidA)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rec.Main, rec.Since, rec.Pane = "working", time.Unix(100, 0), &hint
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}

	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	blocked, release := filepath.Join(root, "attach-write"), filepath.Join(root, "attach-release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	wrap := filepath.Join(root, "bin")
	if err := os.MkdirAll(wrap, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$4\" = set-option ] && [ ! -e '" + release + "' ]; then\n" +
		"  : > '" + blocked + "'\n" +
		"  while [ ! -e '" + release + "' ]; do sleep 0.01; done\n" +
		"fi\n" +
		"exec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))

	env := attach.Env{Log: eventlog.Open(h.state), Store: store}
	done := make(chan error, 1)
	go func() { done <- env.Attach(ctx, w, c, rec, attach.Process) }()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(blocked); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Attach never reached its write")
		}
	}
	if err := store.Lock(ctx, proc.Claude, sidA, func() error {
		cur, ok, err := store.Get(ctx, proc.Claude, sidA)
		if err != nil || !ok {
			return err
		}
		cur.Main, cur.Since = "idle", time.Unix(200, 0)
		return store.Put(ctx, cur)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_since}"); got != "idle|200" {
		t.Fatalf("pane projection = %q, want the record saved during Attach", got)
	}
}

// A title-id attachment on another server clears a process hint, so the next
// hook takes the full path and updates both panes.
func TestOtherServerTitleAttachmentClearsHint(t *testing.T) {
	h := start(t)
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "codex"))
	h.eventually("A ready", func() bool {
		return strings.Contains(h.tmux("capture-pane", "-p", "-t", a), "ready")
	})
	h2 := &harness{t: t, name: "second", dir: h.dir, state: h.state}
	h2.tmux("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", h2.name, "kill-server").Run() })
	b := h2.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_TITLE": sidA[:29] + "..."}, filepath.Join(bin, "codex"))
	h.eventually("B ready", func() bool {
		return strings.Contains(h2.tmux("capture-pane", "-p", "-t", b), "ready")
	})
	if r := h.tmag("", "prune"); r.code != 0 {
		t.Fatal(r.out)
	}
	if evidence := h2.tmux("display-message", "-p", "-t", b, "#{@tmag_evidence}"); evidence != "title-id" {
		t.Fatalf("B evidence = %q", evidence)
	}
	rec, ok, err := (session.Store{Dir: h.state}).Get(context.Background(), "codex", sidA)
	if err != nil || !ok || rec.Pane != nil {
		t.Fatalf("hint after cross-server title attach = %+v, %v, %v", rec.Pane, ok, err)
	}
	h.tmux("send-keys", "-t", a, "/pretool", "Enter")
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
	})
	h.eventually("B working", func() bool {
		return h2.tmux("display-message", "-p", "-t", b, "#{@tmag_state}") == "working"
	})
}

// Two Attaches that captured the same stale world, both from hooks that read
// the same generation, cannot publish a hint that hides the other server.
func TestCrossServerStaleWorldCannotPublishHint(t *testing.T) {
	h := start(t)
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.eventually("A ready", func() bool {
		return strings.Contains(h.tmux("capture-pane", "-p", "-t", a), "ready")
	})
	h2 := &harness{t: t, name: "second", dir: h.dir, state: h.state}
	h2.tmux("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", h2.name, "kill-server").Run() })
	b := h2.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.eventually("B ready", func() bool {
		return strings.Contains(h2.tmux("capture-pane", "-p", "-t", b), "ready")
	})
	ctx := context.Background()
	store := session.Store{Dir: h.state}
	rec, ok, err := store.Get(ctx, "claude", sidA)
	if err != nil || !ok {
		t.Fatal(err)
	}
	sa, sb := tmux.ServerByName(h.name), tmux.ServerByName(h2.name)
	for _, pair := range []struct {
		s tmux.Server
		p string
	}{{sa, a}, {sb, b}} {
		if err := pair.s.ClearPaneOptions(ctx, pair.p); err != nil {
			t.Fatal(err)
		}
	}
	rec.Pane, rec.AttachGeneration = nil, 0
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := w.Find(sa.Socket, a)
	pb, _ := w.Find(sb.Socket, b)
	ca, oka := w.ClientIn(pa)
	cb, okb := w.ClientIn(pb)
	if !oka || !okb {
		t.Fatal("missing clients")
	}
	env := attach.Env{Store: store, Log: eventlog.Open(h.state)}
	if err := env.AttachFromHook(ctx, w, ca, rec, attach.Process, rec.AttachGeneration); err != nil {
		t.Fatal(err)
	}
	if err := env.AttachFromHook(ctx, w, cb, rec, attach.Process, rec.AttachGeneration); err != nil {
		t.Fatal(err)
	}
	current, _, _ := store.Get(ctx, "claude", sidA)
	if current.Pane != nil {
		t.Fatalf("stale-world Attach published hint %+v", current.Pane)
	}
	h2.tmux("send-keys", "-t", b, "/pretool", "Enter")
	h.eventually("B working", func() bool {
		return h2.tmux("display-message", "-p", "-t", b, "#{@tmag_state}") == "working"
	})
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
	})
}

// A canceled Attach cannot leave a visible pane behind: the generation bump
// and hint clear are durable before the pane options are written. The PATH
// wrapper is the barrier at the actual write, not a sleep.
func TestPartialAttachIsNotPublished(t *testing.T) {
	h := start(t)
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.eventually("A ready", func() bool {
		return strings.Contains(h.tmux("capture-pane", "-p", "-t", a), "ready")
	})
	h2 := &harness{t: t, name: "second", dir: h.dir, state: h.state}
	h2.tmux("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", h2.name, "kill-server").Run() })
	b := h2.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	h.eventually("B ready", func() bool {
		return strings.Contains(h2.tmux("capture-pane", "-p", "-t", b), "ready")
	})
	ctx := context.Background()
	store := session.Store{Dir: h.state}
	rec, ok, err := store.Get(ctx, "claude", sidA)
	if err != nil || !ok || rec.Pane == nil {
		t.Fatalf("initial hint = %+v, %v, %v", rec.Pane, ok, err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cb attach.Client
	for _, p := range w.Panes {
		if p.ID == b && p.Server != rec.Pane.Server {
			cb, _ = w.ClientIn(p)
		}
	}
	if cb.Process.PID == 0 {
		t.Fatal("missing B client")
	}
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	blocked, release := filepath.Join(root, "blocked"), filepath.Join(root, "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	wrap := filepath.Join(root, "bin")
	if err := os.MkdirAll(wrap, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = '" + cb.Pane.Server + "' ] && [ \"$4\" = set-option ] && [ ! -e '" + release + "' ]; then\n" +
		"  : > '" + blocked + "'\n" +
		"  while [ ! -e '" + release + "' ]; do sleep 0.01; done\n" +
		"fi\n" +
		"exec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := attach.Env{Store: store, Log: eventlog.Open(h.state)}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- env.Attach(actx, w, cb, rec, attach.Process) }()
	h.eventually("B reached its write", func() bool {
		_, err := os.Stat(blocked)
		return err == nil
	})
	cancel()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("canceled Attach succeeded")
	}
	current, _, _ := store.Get(ctx, "claude", sidA)
	if current.Pane != nil {
		t.Fatalf("canceled Attach kept a hint: %+v", current.Pane)
	}
	if current.AttachGeneration == rec.AttachGeneration {
		t.Fatal("canceled Attach did not advance the generation")
	}
	h.tmux("send-keys", "-t", a, "/pretool", "Enter")
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
	})
}

// A record deleted and recreated cannot make an old hook's generation token
// match a new publication on another server.
func TestEndGenerationABACannotPublishHint(t *testing.T) {
	h := start(t)
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TITLE": sidA[:29] + "..."}, filepath.Join(bin, "codex"))
	h.eventually("A ready", func() bool {
		return strings.Contains(h.tmux("capture-pane", "-p", "-t", a), "ready")
	})
	h2 := &harness{t: t, name: "second", dir: h.dir, state: h.state}
	h2.tmux("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", h2.name, "kill-server").Run() })
	b := h2.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TITLE": sidA[:29] + "..."}, filepath.Join(bin, "codex"))
	h.eventually("B ready", func() bool {
		return strings.Contains(h2.tmux("capture-pane", "-p", "-t", b), "ready")
	})
	ctx := context.Background()
	store := session.Store{Dir: h.state}
	rec, ok, err := store.Get(ctx, "codex", sidA)
	if err != nil || !ok {
		t.Fatal(err)
	}
	sa, sb := tmux.ServerByName(h.name), tmux.ServerByName(h2.name)
	for _, pair := range []struct {
		s tmux.Server
		p string
	}{{sa, a}, {sb, b}} {
		if err := pair.s.ClearPaneOptions(ctx, pair.p); err != nil {
			t.Fatal(err)
		}
	}
	rec.Pane, rec.AttachGeneration = nil, 0
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := w.Find(sa.Socket, a)
	pb, _ := w.Find(sb.Socket, b)
	ca, oka := w.ClientIn(pa)
	cb, okb := w.ClientIn(pb)
	if !oka || !okb {
		t.Fatal("missing clients")
	}
	env := attach.Env{Store: store, Log: eventlog.Open(h.state)}
	if err := env.Attach(ctx, w, ca, rec, attach.Process); err != nil {
		t.Fatal(err)
	}
	attached, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.DetachAll(ctx, attached, rec.Agent, rec.ID); err != nil {
		t.Fatal(err)
	}
	rec, _, err = store.Get(ctx, rec.Agent, rec.ID)
	if err != nil || rec.AttachGeneration == 0 || rec.Pane != nil {
		t.Fatalf("setup: %+v, %v", rec, err)
	}
	w, err = attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := rec.AttachGeneration
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error {
		return store.End(ctx, env.Log, rec, eventlog.Event{Event: eventlog.End})
	}); err != nil {
		t.Fatal(err)
	}
	reborn := rec
	reborn.AttachGeneration = 0
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.Put(ctx, reborn) }); err != nil {
		t.Fatal(err)
	}
	if err := env.Attach(ctx, w, cb, reborn, attach.TitleID); err != nil {
		t.Fatal(err)
	}
	if err := env.AttachFromHook(ctx, w, ca, rec, attach.Process, oldGeneration); err != nil {
		t.Fatal(err)
	}
	current, _, _ := store.Get(ctx, rec.Agent, sidA)
	if current.Pane != nil {
		t.Fatalf("End ABA published hint %+v at generation %d", current.Pane, current.AttachGeneration)
	}
	h.tmux("send-keys", "-t", a, "/pretool", "Enter")
	h.eventually("B working", func() bool {
		return h2.tmux("display-message", "-p", "-t", b, "#{@tmag_state}") == "working"
	})
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
	})
}

// A hook cannot capture a prepared token while the publishing Attach's pane
// is still invisible: the Attach holds the session lock through its write.
func TestPreparePublicationGapCannotPublishHint(t *testing.T) {
	h := start(t)
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.eventually("A ready", func() bool {
		return strings.Contains(h.tmux("capture-pane", "-p", "-t", a), "ready")
	})
	h2 := &harness{t: t, name: "second", dir: h.dir, state: h.state}
	h2.tmux("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", h2.name, "kill-server").Run() })
	b := h2.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA}, filepath.Join(bin, "claude"))
	h.eventually("B ready", func() bool {
		return strings.Contains(h2.tmux("capture-pane", "-p", "-t", b), "ready")
	})
	ctx := context.Background()
	store := session.Store{Dir: h.state}
	rec, ok, err := store.Get(ctx, "claude", sidA)
	if err != nil || !ok || rec.Pane == nil {
		t.Fatalf("initial hint = %+v, %v, %v", rec.Pane, ok, err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cb attach.Client
	for _, p := range w.Panes {
		if p.ID == b && p.Server != rec.Pane.Server {
			cb, _ = w.ClientIn(p)
		}
	}
	if cb.Process.PID == 0 {
		t.Fatal("missing B client")
	}
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	blocked, release := filepath.Join(root, "blocked"), filepath.Join(root, "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	wrap := filepath.Join(root, "bin")
	if err := os.MkdirAll(wrap, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = '" + cb.Pane.Server + "' ] && [ \"$4\" = set-option ] && [ ! -e '" + release + "' ]; then\n" +
		"  : > '" + blocked + "'\n" +
		"  while [ ! -e '" + release + "' ]; do sleep 0.01; done\n" +
		"fi\n" +
		"exec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := attach.Env{Store: store, Log: eventlog.Open(h.state)}
	actx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- env.Attach(actx, w, cb, rec, attach.Process) }()
	h.eventually("B reached its write", func() bool {
		_, err := os.Stat(blocked)
		return err == nil
	})
	// The session lock is held across the pane write; a bounded probe cannot
	// enter until the barrier is released.
	probeCtx, probeCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	entered := false
	probeErr := store.Lock(probeCtx, "claude", sidA, func() error { entered = true; return nil })
	probeCancel()
	if entered || !errors.Is(probeErr, context.DeadlineExceeded) {
		t.Fatalf("session lock accessible before the pane write: entered=%v err=%v", entered, probeErr)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var captured session.Record
	if err := store.Lock(actx, "claude", sidA, func() error {
		var err error
		captured, _, err = store.Get(actx, "claude", sidA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	frozen, err := attach.Load(actx)
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := frozen.Find(rec.Pane.Server, a)
	ca, ok := frozen.ClientIn(pa)
	if !ok {
		t.Fatal("missing A client")
	}
	if err := env.AttachFromHook(actx, frozen, ca, captured, attach.Process, captured.AttachGeneration); err != nil {
		t.Fatal(err)
	}
	current, _, _ := store.Get(ctx, "claude", sidA)
	if current.Pane != nil {
		t.Fatalf("a hook captured a token before the pane was visible: %+v", current.Pane)
	}
	h.tmux("send-keys", "-t", a, "/pretool", "Enter")
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
	})
	h.eventually("B follows A", func() bool {
		return h2.tmux("display-message", "-p", "-t", b, "#{@tmag_state}") == "working"
	})
}

// A failed replacement must not log a Detach for a pane that still shows the
// old session.
func TestFailedReplacementDoesNotLogDetach(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.eventually("ready", func() bool {
		return strings.Contains(h.tmux("capture-pane", "-p", "-t", pane), "ready")
	})
	ctx := context.Background()
	store := session.Store{Dir: h.state}
	rec, ok, err := store.Get(ctx, "claude", sidA)
	if err != nil || !ok {
		t.Fatalf("missing record: %v", err)
	}
	next := rec
	next.ID, next.Pane = sidB, nil
	if err := store.Put(ctx, next); err != nil {
		t.Fatal(err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := w.Find(tmux.ServerByName(h.name).Socket, pane)
	if !ok {
		t.Fatal("pane missing")
	}
	c, ok := w.ClientIn(p)
	if !ok {
		t.Fatal("client missing")
	}
	log := eventlog.Open(h.state)
	before, err := log.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	if len(before) > 0 {
		seq = before[len(before)-1].Seq
	}
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrap := t.TempDir()
	script := "#!/bin/sh\nif [ \"$4\" = set-option ]; then exit 17; fi\nexec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := (attach.Env{Store: store, Log: log}).Attach(ctx, w, c, next, attach.Process); err == nil {
		t.Fatal("expected a write failure")
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_session}"); got != sidA {
		t.Fatalf("old pane changed: %q", got)
	}
	events, err := log.Since(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Event == eventlog.Detach && e.Session == sidA {
			t.Fatalf("unchanged pane emitted Detach: %+v", e)
		}
	}
}

// A multiline notifier is executable command data, not a display field: a
// hinted hook must pass it through without stripping its newlines.
func TestHintedNotifyPreservesNewlines(t *testing.T) {
	h := start(t)
	sent := filepath.Join(h.dir, "notified")
	h.tmux("set-option", "-g", "@tmag-alert", "finished")
	h.tmux("set-option", "-g", "@tmag-notify", "# notify when away\nprintf '%s' \"$TMAG_ALERT\" > "+sent)
	if r := h.tmag("", "notify"); r.code != 0 {
		t.Fatalf("manual notify: %d %s", r.code, r.out)
	}
	if b, err := os.ReadFile(sent); err != nil || string(b) != "test" {
		t.Fatalf("manual notify: %q %v", b, err)
	}
	_ = os.Remove(sent)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	if r := h.tmag("", "send", sidA[:8], "--wait", "work"); r.code != 0 {
		t.Fatal(r)
	}
	h.eventually("configured finished notification", func() bool {
		b, err := os.ReadFile(sent)
		return err == nil && string(b) == "finished"
	})
}
