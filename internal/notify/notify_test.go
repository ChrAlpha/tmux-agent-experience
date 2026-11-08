package notify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/admission"
	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
)

func TestAlertText(t *testing.T) {
	blocked := Alert{Kind: "blocked", Agent: "claude", StateKind: "permission", Request: "3f9c2a01b6d4", RequestKind: "permission",
		Tool: "Bash", Cwd: "/home/me/src/api", Target: "work:1.0", Host: "box"}
	if got := blocked.Title(); got != "claude needs your approval" {
		t.Errorf("title = %q", got)
	}
	if got := blocked.Message(); got != "Bash · api · work:1.0 · box" {
		t.Errorf("message = %q", got)
	}
	question := Alert{Kind: "blocked", Agent: "claude", RequestKind: "question", Tool: "AskUserQuestion"}
	if got := question.Title(); got != "claude has a question" {
		t.Errorf("question title = %q", got)
	}
	failed := Alert{Kind: "finished", Agent: "codex", StateKind: "error", Cwd: "/w/web", Host: "box"}
	if got, msg := failed.Title(), failed.Message(); got != "codex stopped on an error" || msg != "web · box" {
		t.Errorf("failed turn: %q / %q", got, msg)
	}
	if got := (Alert{Kind: Test, Host: "box"}).Message(); got != "a test alert from tmag notify · box" {
		t.Errorf("test message = %q", got)
	}

	env := blocked.Env()
	for _, want := range []string{"TMAG_ALERT=blocked", "TMAG_REQUEST=3f9c2a01b6d4", "TMAG_TOOL=Bash", "TMAG_TARGET=work:1.0", "TMAG_TITLE=claude needs your approval"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %v", want, env)
		}
	}
}

func TestFromEnvTakesTheCommandOut(t *testing.T) {
	command, env := FromEnv([]string{"PATH=/bin", commandVar + "=curl -d hi ntfy.sh/x", "TMAG_ALERT=blocked"})
	if command != "curl -d hi ntfy.sh/x" || !slices.Equal(env, []string{"PATH=/bin", "TMAG_ALERT=blocked"}) {
		t.Fatalf("command %q, env %v", command, env)
	}
}

func TestRunGivesTheVariablesAndTheOutput(t *testing.T) {
	out, err := Run(context.Background(), `printf '%s|%s' "$TMAG_ALERT" "$TMAG_TOOL"; exit 3`, append(os.Environ(), Alert{Kind: "blocked", Tool: "Bash"}.Env()...))
	var exit interface{ ExitCode() int }
	if string(out) != "blocked|Bash" || !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("out %q, err %v", out, err)
	}
}

// A command that hangs is ended with everything it started, so that a
// notifier stuck on the network leaves nothing behind.
func TestRunEndsTheWholeCommandInTime(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, `sleep 60 & echo $! > `+pidFile+`; wait`, os.Environ())
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for deadline := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the command's child %d outlived it", pid)
		}
	}
}

func TestRunAlertDescribesAFailure(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	environ := append(os.Environ(), commandVar+"=echo unreachable >&2; exit 7")
	err := RunAlert(t.TempDir(), environ)
	if err == nil || !strings.Contains(err.Error(), "@tmag-notify") || strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
	if err := RunAlert(t.TempDir(), os.Environ()); err == nil {
		t.Fatal("no command, no error")
	}
}

func TestNotificationLeaseSurvivesParentCloseAndOrdinaryShellChild(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	// Occupy the default four positions. The shell inherits one of those
	// descriptions and then starts an ordinary descendant with the same FD.
	var leases []*admission.Lease
	for range 4 {
		l, err := admission.Acquire(ctx, root, admission.Notify)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		defer l.Close()
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd, cleanup := helper.Command(ctx, "/bin/sh", "-c", `/bin/sh -c 'echo ready > "$1"; sleep 60' child "$1" & wait`, "parent", ready)
	defer cleanup()
	cmd.ExtraFiles = []*os.File{leases[0].File()}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup()
		if err := cmd.Wait(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != -1 {
				t.Errorf("unexpected owned shell cleanup result: %v", err)
			}
		}
	}()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("descendant did not start", ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if err := leases[0].Close(); err != nil {
		t.Fatal(err)
	}
	if next, err := admission.Acquire(ctx, root, admission.Notify); next != nil || !errors.Is(err, admission.ErrLimit) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("shell lost inherited capacity: %v", err)
	}
	cleanup()
}

func TestRunAlertRejectsUnprovenInheritanceWithoutClosingCallerFD(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	for _, slot := range []string{"0", "invalid"} {
		t.Run(slot, func(t *testing.T) {
			sentinel, err := os.CreateTemp(t.TempDir(), "sentinel")
			if err != nil {
				t.Fatal(err)
			}
			defer sentinel.Close()
			env := append(os.Environ(), commandVar+"=true", slotVar+"="+slot)
			if err := runAlert(t.TempDir(), env, int(sentinel.Fd())); err != nil {
				t.Fatal("normal admission after rejected inheritance failed", err)
			}
			if _, err := sentinel.WriteString("original descriptor remains live"); err != nil {
				t.Fatal("unproven metadata closed caller FD", err)
			}
		})
	}
}

func TestRunAlertWithAbsentCandidateAcquiresNormally(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	sentinel, err := os.CreateTemp(t.TempDir(), "sentinel")
	if err != nil {
		t.Fatal(err)
	}
	defer sentinel.Close()
	root := t.TempDir()
	env := append(os.Environ(), commandVar+"=true", slotVar+"=0")
	const missing = 1<<31 - 1
	var descriptorLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &descriptorLimit); err != nil {
		t.Fatal(err)
	}
	if missing <= descriptorLimit.Cur {
		t.Fatal("invalid-FD fixture must exceed the process limit")
	}

	if err := runAlert(root, env, missing); err != nil {
		t.Fatal("absent candidate could not use normal admission", err)
	}
	if _, err := sentinel.Stat(); err != nil {
		t.Fatal("unrelated original closed", err)
	}
	// Neither rejected candidate setup nor the command may leak a reservation.
	for range 4 {
		lease, err := admission.Acquire(context.Background(), root, admission.Notify)
		if err != nil {
			t.Fatal("reservation leaked", err)
		}
		defer lease.Close()
	}
}

func TestRunAlertValidInheritedLeaseDoesNotReserveTwice(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	root := t.TempDir()
	ctx := context.Background()
	var holders []*admission.Lease
	for range 4 {
		lease, err := admission.Acquire(ctx, root, admission.Notify)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		holders = append(holders, lease)
	}
	inherited := holders[0]
	env := append(os.Environ(), commandVar+"=true", slotVar+"="+strconv.Itoa(inherited.Slot()))
	if err := runAlert(root, env, int(inherited.File().Fd())); err != nil {
		t.Fatal("valid inheritance tried to reserve again at full capacity", err)
	}
	if _, err := inherited.File().Stat(); err != nil {
		t.Fatal("RunAlert consumed caller's original reservation", err)
	}
	if extra, err := admission.Acquire(ctx, root, admission.Notify); extra != nil || !errors.Is(err, admission.ErrLimit) {
		if extra != nil {
			_ = extra.Close()
		}
		t.Fatalf("inherited holder released prematurely: %v", err)
	}
}
