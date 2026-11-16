package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// An older hook cannot overwrite a projection saved by a newer one: both
// serialize on the pane lock and each projects the record it reads there.
func TestProjectHintReadsLatestUnderPaneLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	store := session.Store{Dir: root}
	hint := session.PaneHint{Server: "/tmp/tmux-test.sock", ID: "%1", PID: 10, Start: "10"}
	rec := session.Record{Agent: proc.Claude, ID: "s", Main: "working", Pane: &hint, Since: time.Unix(100, 0)}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	env := attach.Env{Log: eventlog.Open(root), Store: store}

	old := writeProjection
	t.Cleanup(func() { writeProjection = old })
	var mu sync.Mutex
	var projections []attach.Projection
	entered, release := make(chan struct{}), make(chan struct{})
	late := make(chan struct{})
	writeProjection = func(_ context.Context, h session.PaneHint, _, _ string, pr attach.Projection, _ bool) (tmux.Pane, string, string, bool, bool, bool) {
		mu.Lock()
		n := len(projections)
		projections = append(projections, pr)
		mu.Unlock()
		if n == 0 {
			close(entered)
			<-release
		} else {
			close(late)
		}
		return tmux.Pane{Server: h.Server, ID: h.ID}, "", "", false, false, true
	}

	type result struct{ err error }
	hook := func(done chan<- result) {
		_, _, _, _, _, _, _, err := projectHint(ctx, env, proc.Claude, "s", hint, false)
		done <- result{err}
	}
	first := make(chan result, 1)
	go hook(first)
	<-entered // A holds the pane lock, after reading the working record.

	// B saves a newer record while A is paused in its writer.
	if err := store.Lock(ctx, proc.Claude, "s", func() error {
		cur, ok, err := store.Get(ctx, proc.Claude, "s")
		if err != nil || !ok {
			return err
		}
		cur.Main, cur.Since = "idle", time.Unix(200, 0)
		return store.Put(ctx, cur)
	}); err != nil {
		t.Fatal(err)
	}
	second := make(chan result, 1)
	go hook(second)
	select {
	case <-late:
		t.Fatal("the second hook projected while the first held the pane lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if r := <-first; r.err != nil {
		t.Fatal(r.err)
	}
	if r := <-second; r.err != nil {
		t.Fatal(r.err)
	}
	got, ok, err := store.Get(ctx, proc.Claude, "s")
	if err != nil || !ok || got.Main != "idle" {
		t.Fatalf("latest record = %+v, %v, %v", got, ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(projections) != 2 || projections[0].State != "working" || projections[1].State != "idle" {
		t.Fatalf("projections = %+v", projections)
	}
}

// A hook and attach.Show serialize on the pane lock: Show has read an older
// record, a hook saves a newer one, and the hook cannot write until Show
// releases the lock; both then read the latest record.
func TestProjectHintAndShowShareThePaneLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv, pane := startTmuxPane(t)
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := session.Store{Dir: root}
	hint := session.PaneHint{Server: srv.Socket, ID: pane, PID: 10, Start: "10"}
	rec := session.Record{Agent: proc.Claude, ID: "s", Main: "working", Pane: &hint, Since: time.Unix(100, 0)}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	a := attach.Attachment{Agent: proc.Claude, Session: "s", PID: 10, Start: "10", Evidence: attach.Process}
	if err := srv.SetPaneOptions(ctx, pane, map[string]string{
		"agent": a.Agent, "session": a.Session, "pid": "10", "start": a.Start,
		"evidence": a.Evidence, "state": "working", "since": "100", "cmd": "sh",
	}, nil); err != nil {
		t.Fatal(err)
	}
	env := attach.Env{Log: eventlog.Open(root), Store: store}

	// A PATH wrapper holds Show's first set-option after it has read the old
	// record. The hook's own tmux call starts with `if`, so it is not held.
	blockedFile := filepath.Join(root, "show-read")
	releaseFile := filepath.Join(root, "show-release")
	t.Cleanup(func() { _ = os.WriteFile(releaseFile, nil, 0o600) })
	wrap := filepath.Join(root, "bin")
	if err := os.MkdirAll(wrap, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$4\" = set-option ] && [ ! -e '" + releaseFile + "' ]; then\n" +
		"  : > '" + blockedFile + "'\n" +
		"  while [ ! -e '" + releaseFile + "' ]; do sleep 0.01; done\n" +
		"fi\n" +
		"exec '" + strings.ReplaceAll(realTmux, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))

	showDone := make(chan error, 1)
	go func() { showDone <- env.Show(ctx, tmux.Pane{Server: srv.Socket, ID: pane}, a) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(blockedFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Show never reached its write")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Show has read old working and is holding the pane lock in set-option.
	if err := store.Lock(ctx, proc.Claude, "s", func() error {
		cur, ok, err := store.Get(ctx, proc.Claude, "s")
		if err != nil || !ok {
			return err
		}
		cur.Main, cur.Since = "idle", time.Unix(200, 0)
		return store.Put(ctx, cur)
	}); err != nil {
		t.Fatal(err)
	}
	realWriteProjection := writeProjection
	oldWriteProjection := writeProjection
	writing := make(chan struct{})
	writeProjection = func(ctx context.Context, h session.PaneHint, agent, id string, pr attach.Projection, alert bool) (tmux.Pane, string, string, bool, bool, bool) {
		close(writing)
		return realWriteProjection(ctx, h, agent, id, pr, alert)
	}
	t.Cleanup(func() { writeProjection = oldWriteProjection })
	hookDone := make(chan error, 1)
	go func() {
		_, _, _, _, _, _, _, err := projectHint(ctx, env, proc.Claude, "s", hint, false)
		hookDone <- err
	}()
	select {
	case <-writing:
		t.Fatal("the hook wrote while Show held the pane lock")
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-showDone; err != nil {
		t.Fatal(err)
	}
	if err := <-hookDone; err != nil {
		t.Fatal(err)
	}
	out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_since}")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "idle|200\n" {
		t.Fatalf("pane projection = %q, want the latest record", got)
	}
}
