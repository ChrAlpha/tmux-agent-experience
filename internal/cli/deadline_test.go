package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestHookCLIInheritedInput(t *testing.T) {
	if command := os.Getenv("TMAG_DEADLINE_TEST_COMMAND"); command != "" {
		os.Exit(Main([]string{command, "claude"}))
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"hook", "answer-hook"} {
		for _, signaled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/signal=%t", command, signaled), func(t *testing.T) {
				dir := t.TempDir()
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				defer w.Close()
				// Retain a reference to the child's read-end status flags. This
				// observes Main installing its cancellation-owned stream before
				// sending a signal, without depending on a startup sleep.
				fd := r.Fd()
				if err := syscall.SetNonblock(int(fd), false); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestHookCLIInheritedInput$")
				cmd.Env = append(os.Environ(), "TMAG_DEADLINE_TEST_COMMAND="+command,
					"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0",
					"TMUX=", "TMUX_PANE=", "TMUX_TMPDIR="+filepath.Join(dir, "tmux"),
					"TMAG_STATE_DIR="+filepath.Join(dir, "state"), "TMAG_AUTH_DIR="+filepath.Join(dir, "auth"))
				var stdout, stderr bytes.Buffer
				cmd.Stdin, cmd.Stdout, cmd.Stderr = r, &stdout, &stderr
				started := time.Now()
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waited := make(chan error, 1)
				go func() { waited <- cmd.Wait() }()
				t.Cleanup(func() { cancel(); _ = cmd.Process.Kill() })
				if _, err := io.WriteString(w, `{"session_id":"never-apply","hook_event_name":"SessionStart"}`); err != nil {
					t.Fatal(err)
				}
				if signaled {
					deadline := time.NewTimer(2 * time.Second)
					defer deadline.Stop()
					tick := time.NewTicker(time.Millisecond)
					defer tick.Stop()
					for {
						flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
						if errno != 0 {
							t.Fatal(errno)
						}
						if flags&syscall.O_NONBLOCK != 0 {
							break
						}
						select {
						case <-tick.C:
						case <-deadline.C:
							t.Fatal("hook did not establish owned stdin")
						}
					}
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-waited:
					if err != nil {
						t.Fatalf("exit=%v stderr=%s", err, stderr.String())
					}
				case <-ctx.Done():
					t.Fatal("hook outlived its input budget")
				}
				if elapsed := time.Since(started); elapsed > hook.Deadline+500*time.Millisecond {
					t.Fatalf("hook took %s", elapsed)
				}
				if stdout.Len() != 0 {
					t.Fatalf("fail-open stdout=%q", stdout.String())
				}
				// Complete the payload after exit: it must not have been handed
				// to an abandoned reader that can apply it later.
				_, _ = io.WriteString(w, "\n")
				if records, _, err := (session.Store{Dir: filepath.Join(dir, "state")}).All(context.Background()); err != nil || len(records) != 0 {
					t.Fatalf("late records=%v err=%v", records, err)
				}
			})
		}
	}
}

func TestWatchCancelsOwnedBlockedOutput(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	{
		t.Run("snapshot", func(t *testing.T) {
			log := eventlog.Open(t.TempDir())
			log.MaxBytes = 64 << 20
			store := session.Store{Dir: log.Dir}
			large := strings.Repeat("x", 2<<20)
			for i := range 2 {
				id := fmt.Sprintf("large-%d", i)
				if err := store.Put(context.Background(), session.Record{Agent: "claude", ID: id, Main: "idle", Cwd: large}); err != nil {
					t.Fatal(err)
				}
				ev := eventlog.Event{Agent: "claude", Session: id, Event: "SessionStart", Cwd: large}
				if err := log.Append(context.Background(), &ev); err != nil {
					t.Fatal(err)
				}
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			a := app{ctx: ctx, log: log, stdout: w, stderr: &stderr}
			args := []string{"watch"}
			done := make(chan int, 1)
			go func() { done <- a.run(args) }()
			first := make(chan []byte, 1)
			go func() { b := make([]byte, 1); _, _ = io.ReadFull(r, b); first <- b }()
			var prefix []byte
			select {
			case prefix = <-first:
			case <-time.After(5 * time.Second):
				cancel()
				_ = w.Close()
				t.Fatal("watch produced no frame")
			}
			// The first multi-megabyte frame has entered the pipe, whose reader
			// remains alive and stops draining here.
			cancel()
			select {
			case code := <-done:
				if code != ExitOK {
					t.Fatalf("exit=%d stderr=%s", code, stderr.String())
				}
			case <-time.After(time.Second):
				_ = w.Close()
				<-done
				t.Fatal("watch remained blocked on its reader")
			}
			_ = w.Close()
			rest, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			prefix = append(prefix, rest...)
			if json.Valid(bytes.TrimSpace(prefix)) || len(prefix) >= len(large) {
				t.Fatalf("finished frame after cancellation (%d bytes)", len(prefix))
			}
		})
	}
}

func TestAnswerWaitOutlivesSetupBudgetAndCancelsSilently(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setup, endSetup := context.WithCancel(ctx)
	defer endSetup()
	ready := make(chan struct{})
	var stdout bytes.Buffer
	state, err := os.MkdirTemp("", "tmag-ad-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state)
	log := eventlog.Open(state)
	a := app{ctx: ctx, setup: setup, setupDone: func() { endSetup(); close(ready) }, log: log, stdout: &stdout,
		stdin: strings.NewReader(`{"session_id":"answer-deadline","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"memory only"}}`)}
	done := make(chan error, 1)
	go func() { done <- a.answerHook() }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("setup failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not finish")
	}
	rec, ok, err := (session.Store{Dir: log.Dir}).Get(context.Background(), "claude", "answer-deadline")
	if err != nil || !ok || len(rec.Requests) != 1 {
		t.Fatalf("request=%+v err=%v", rec, err)
	}
	probe, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := answer.Show(probe, log.Dir, rec.Requests[0].ID); err != nil {
		t.Fatalf("setup cancellation ended decision wait: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("decision wait ignored cancellation")
	}
	if stdout.Len() != 0 {
		t.Fatalf("canceled answer emitted %q", stdout.String())
	}
	if err := answer.Decide(probe, log.Dir, rec.Requests[0].ID, answer.Decision{Behavior: answer.Allow}); err == nil {
		t.Fatal("late decision accepted")
	}
}
