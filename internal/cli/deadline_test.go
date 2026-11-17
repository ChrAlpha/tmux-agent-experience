package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
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
				if signaled {
					data, err := os.ReadFile(filepath.Join(dir, "state", "hook.log"))
					if err != nil {
						t.Fatalf("canceled hook was not logged: %v", err)
					}
					lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
					fields := strings.Fields(lines[0])
					if len(lines) != 1 || len(fields) != 3 || fields[1] != "claude" || fields[2] != "canceled" {
						t.Fatalf("canceled log = %q", data)
					}
					if _, err := time.Parse(time.RFC3339, fields[0]); err != nil || strings.Contains(lines[0], "never-apply") {
						t.Fatalf("canceled log is not metadata only: %q", lines[0])
					}
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

// A hook that runs out of its setup budget still reaches hook.log from the
// reserve before the hard cutoff; a successful slow hook is logged as slow.
func TestHookReportsTimeoutsAndSlowHooks(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	a := app{ctx: context.Background(), log: eventlog.Open(root), stdin: strings.NewReader("")}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	a.setup = expired
	if code := a.cmdHook([]string{"claude"}); code != ExitOK {
		t.Fatalf("timed out hook exit = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(root, "hook.log"))
	if err != nil || !strings.Contains(string(data), " claude timeout\n") {
		t.Fatalf("timeout log = %q, %v", data, err)
	}

	root = t.TempDir()
	slow := app{ctx: context.Background(), log: eventlog.Open(root),
		stdin:   strings.NewReader(`{"session_id":"slow","hook_event_name":"Ignored"}`),
		started: time.Now().Add(-2 * time.Second)}
	if code := slow.cmdHook([]string{"claude"}); code != ExitOK {
		t.Fatalf("slow hook exit = %d", code)
	}
	data, err = os.ReadFile(filepath.Join(root, "hook.log"))
	if err != nil || !strings.Contains(string(data), " claude slow\n") {
		t.Fatalf("slow log = %q, %v", data, err)
	}
}

// sessionStripe mirrors session.Store's lock stripe, so a test can hold one
// session's lock without holding the hook's own.
func sessionStripe(agent, id string) byte {
	sum := sha256.Sum256([]byte("session\x00" + agent + "\x00" + id))
	return sum[0]
}

// A hook whose body succeeds but whose deferred reconciliation runs out of
// budget reports a timeout from the reserve after the setup deadline, with
// its own record already saved.
func TestDeferredReconciliationTimeoutReachesHookLog(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	log := eventlog.Open(root)
	store := session.Store{Dir: root}
	hookID := "hook-session"
	goneID := "gone-session"
	for sessionStripe(proc.Claude, goneID) == sessionStripe(proc.Claude, hookID) {
		goneID += "x"
	}
	gone := session.Record{Agent: proc.Claude, ID: goneID, Main: adapter.Idle,
		Host: session.Host{PID: 1 << 30, Start: "s-gone"}}
	if err := store.Put(context.Background(), gone); err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- store.Lock(context.Background(), proc.Claude, goneID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	t.Cleanup(func() {
		close(release)
		if err := <-locked; err != nil {
			t.Errorf("release held lock: %v", err)
		}
	})

	setup, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	a := app{ctx: context.Background(), setup: setup, log: log,
		stdin: strings.NewReader(fmt.Sprintf(`{"session_id":%q,"hook_event_name":"SessionStart","source":"startup"}`, hookID))}
	started := time.Now()
	if code := a.cmdHook([]string{proc.Claude}); code != ExitOK {
		t.Fatalf("hook exit = %d", code)
	}
	if elapsed := time.Since(started); elapsed >= hook.Deadline {
		t.Fatalf("hook took %s, past its hard deadline", elapsed)
	}
	data, err := os.ReadFile(filepath.Join(root, "hook.log"))
	if err != nil || strings.Count(string(data), " claude timeout\n") != 1 {
		t.Fatalf("timeout log = %q, %v", data, err)
	}
	if strings.Contains(string(data), " claude slow\n") {
		t.Fatalf("timed-out hook also logged slow: %q", data)
	}
	rec, ok, err := store.Get(context.Background(), proc.Claude, hookID)
	if err != nil || !ok || rec.State() != adapter.Idle {
		t.Fatalf("state before reconciliation = %+v, %v, %v", rec, ok, err)
	}
	if _, ok, err := store.Get(context.Background(), proc.Claude, goneID); !ok || err != nil {
		t.Fatalf("busy session ended despite its held lock: ok=%v err=%v", ok, err)
	}
}

// An answer hook reports a slow setup at the boundary where it starts waiting
// for a decision; the wait itself never counts.
func TestAnswerHookReportsSlowSetupAtTheWaitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started time.Time
		wait    time.Duration
		slow    bool
	}{
		{"slow setup", time.Now().Add(-2 * time.Second), 0, true},
		{"fast setup and long wait", time.Now(), 600 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", "")
			t.Setenv("TMUX_PANE", "")
			t.Setenv("TMUX_TMPDIR", t.TempDir())
			root := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			setup, endSetup := context.WithCancel(ctx)
			defer endSetup()
			ready := make(chan struct{})
			a := app{ctx: ctx, setup: setup, log: eventlog.Open(root), started: tc.started,
				stdout: &bytes.Buffer{}, setupDone: func() { endSetup(); close(ready) },
				stdin: strings.NewReader(`{"session_id":"answer-boundary","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"true"}}`)}
			done := make(chan error, 1)
			go func() { done <- a.answerHook() }()
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("setup failed: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("setup did not finish")
			}
			rec, ok, err := (session.Store{Dir: root}).Get(context.Background(), proc.Claude, "answer-boundary")
			if err != nil || !ok || len(rec.Requests) != 1 {
				t.Fatalf("request = %+v, %v, %v", rec, ok, err)
			}
			probe, stop := context.WithTimeout(context.Background(), time.Second)
			if _, err := answer.Show(probe, root, rec.Requests[0].ID); err != nil {
				stop()
				t.Fatalf("hook did not enter its decision wait: %v", err)
			}
			stop()
			if tc.wait > 0 {
				time.Sleep(tc.wait)
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
			data, err := os.ReadFile(filepath.Join(root, "hook.log"))
			got := strings.Count(string(data), " claude slow\n")
			if tc.slow && got != 1 {
				t.Fatalf("slow entries = %d, want 1: %q, %v", got, data, err)
			}
			if !tc.slow && got != 0 {
				t.Fatalf("decision wait counted as slow setup: %q", data)
			}
		})
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

// A command that cannot heed its context's cancellation, blocked on its
// input here, outlives a first signal and ends by the second, as any
// program would.
func TestASecondSignalEndsACommand(t *testing.T) {
	if os.Getenv("TMAG_SIGNAL_TEST") != "" {
		_, stop := signalContext()
		defer stop()
		defer endOnSecondSignal()()
		fmt.Println("ready")
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command(exe, "-test.run=^TestASecondSignalEndsACommand$")
	cmd.Env = append(os.Environ(), "TMAG_SIGNAL_TEST=1")
	cmd.Stdin = r
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child: %q, %v", line, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		t.Fatalf("a first signal ended a command that cannot heed it: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("a second signal did not end the command")
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("the command ended %v, not by the second signal", cmd.ProcessState)
	}
}
