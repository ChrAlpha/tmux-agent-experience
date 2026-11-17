package e2e

import (
	"context"
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

// listsPane reports whether rec lists pane for a proven client.
func listsPane(rec session.Record, pane string) bool {
	for _, s := range rec.Shown {
		if s.Pane == pane && s.Start != "" {
			return true
		}
	}
	return false
}

// A session's state hooks start no tmux process when the projection stays
// the same, and exactly one for the pane its record lists when it changes.
func TestListedHookUpdatesItsPaneWithOneTmuxProcess(t *testing.T) {
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
	h.eventually("the pane listed", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && listsPane(rec, pane)
	})
	// SessionStart lists the pane before it finishes; wait for it to exit so
	// its remaining reads are not counted with the first prompt.
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

	// A finishing hook reuses the options its pane's server printed with the
	// guarded write and reads each other server once: three servers cost
	// three tmux processes, not four.
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

// The guard writes nothing when the pane's attachment changed under its
// listing; the hook unlists it and takes the full proof path, which restores
// the pane and lists it again.
func TestGuardRefusesAChangedAttachment(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "100"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	store := session.Store{Dir: h.state}
	h.eventually("the pane listed", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && listsPane(rec, pane)
	})
	h.tmux("set-option", "-p", "-t", pane, "@tmag_session", sidB)
	h.tmux("send-keys", "-t", pane, "/pretool", "Enter")
	h.eventually("the full path restored the attachment", func() bool {
		return h.tmux("display-message", "-p", "-t", pane, "#{@tmag_session}|#{@tmag_state}|#{@tmag_kind}") == sidA+"|working|"
	})
	h.eventually("the full path listed the pane again", func() bool {
		rec, ok, err := store.Get(context.Background(), proc.Claude, sidA)
		return err == nil && ok && listsPane(rec, pane)
	})
}

// A session shown in two panes is projected onto both by a fast hook, without
// waiting for a reconciling read.
func TestSharedSessionProjection(t *testing.T) {
	h := start(t)
	// High pane ids also exercise pane ids in the guarded call.
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

// A pane whose proven client died is not projected or belled: its options
// still name the session, but it no longer runs the program it was attached
// with, so the guard refuses it.
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

// Attach lists the pane and writes it under the session's lock: a hook that
// saves a newer state meanwhile waits for it, then finds the pane listed, so
// its projection reaches the pane.
func TestHookAfterAttachFindsThePaneListed(t *testing.T) {
	h := start(t)
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	rec, ok, err := store.Get(ctx, proc.Claude, sidA)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rec.Main, rec.Since, rec.Shown = "working", time.Unix(100, 0), nil
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
	attached := make(chan error, 1)
	go func() { attached <- env.Attach(ctx, w, c, rec, attach.Process) }()
	h.eventually("Attach reached its write", func() bool {
		_, err := os.Stat(blocked)
		return err == nil
	})
	saved := make(chan error, 1)
	go func() {
		saved <- store.Lock(ctx, proc.Claude, sidA, func() error {
			cur, ok, err := store.Get(ctx, proc.Claude, sidA)
			if err != nil || !ok {
				return err
			}
			cur.Main, cur.Since = "idle", time.Unix(200, 0)
			return store.Put(ctx, cur)
		})
	}()
	select {
	case err := <-saved:
		t.Fatalf("a hook saved while Attach wrote the pane: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	latest, _, err := store.Get(ctx, proc.Claude, sidA)
	if err != nil || !listsPane(latest, pane) {
		t.Fatalf("the hook found the pane unlisted: %+v, %v", latest.Shown, err)
	}
	if _, refused, err := env.Project(ctx, latest); err != nil || len(refused) != 0 {
		t.Fatalf("Project: refused %+v, %v", refused, err)
	}
	if got := h.tmux("display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_since}"); got != "idle|200" {
		t.Fatalf("pane projection = %q, want the record saved after Attach", got)
	}
}

// A title-id attachment on another server is listed too, so a hook that
// updates its own pane updates that one as well.
func TestOtherServerTitleAttachmentIsProjected(t *testing.T) {
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
	if err != nil || !ok || !listsPane(rec, a) || !listsPane(rec, b) {
		t.Fatalf("listed after the cross-server title attach = %+v, %v, %v", rec.Shown, ok, err)
	}
	h.tmux("send-keys", "-t", a, "/pretool", "Enter")
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
	})
	h.eventually("B working", func() bool {
		return h2.tmux("display-message", "-p", "-t", b, "#{@tmag_state}") == "working"
	})
}

// A canceled Attach cannot leave a pane showing the session that its record
// does not list: the listing is durable before the pane options are written.
// The PATH wrapper is the barrier at the actual write, not a sleep.
func TestCanceledAttachLeavesItsPaneListed(t *testing.T) {
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
	if err != nil || !ok || !listsPane(rec, a) {
		t.Fatalf("initial listing = %+v, %v, %v", rec.Shown, ok, err)
	}
	w, err := attach.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := h2.tmux("display-message", "-p", "#{socket_path}")
	var cb attach.Client
	for _, p := range w.Panes {
		if p.ID == b && p.Server == second {
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
	if h2.tmux("display-message", "-p", "-t", b, "#{@tmag_session}") == sidA && !listsPane(current, b) {
		t.Fatalf("a pane shows the session its record does not list: %+v", current.Shown)
	}
	h.tmux("send-keys", "-t", a, "/pretool", "Enter")
	h.eventually("A working", func() bool {
		return h.tmux("display-message", "-p", "-t", a, "#{@tmag_state}") == "working"
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
	next.ID, next.Shown = sidB, nil
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
// hook that updates its listed pane must pass it through without stripping
// its newlines.
func TestFastHookNotifyPreservesNewlines(t *testing.T) {
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
