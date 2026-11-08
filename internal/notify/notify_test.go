package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
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
	out, err := Run(context.Background(), t.TempDir(), `printf '%s|%s' "$TMAG_ALERT" "$TMAG_TOOL"; exit 3`, append(os.Environ(), Alert{Kind: "blocked", Tool: "Bash"}.Env()...))
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
	_, err := Run(ctx, t.TempDir(), `sleep 60 & echo $! > `+pidFile+`; wait`, os.Environ())
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
	environ := append(os.Environ(), commandVar+"=echo unreachable >&2; exit 7")
	err := RunAlert(t.TempDir(), environ)
	if err == nil || !strings.Contains(err.Error(), "@tmag-notify") || strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
	if err := RunAlert(t.TempDir(), os.Environ()); err == nil {
		t.Fatal("no command, no error")
	}
}

func TestExternalExecutionLimitIsLocalAndDoesNotQueue(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := coord.Current(ctx, root); err != nil {
		t.Fatal(err)
	}
	var held []*lockfile.Held
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	for i := range Executions {
		l, err := lockfile.AcquireWithin(ctx, root, fmt.Sprintf("notify-%d.lock", i), false, false)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
	}
	marker := filepath.Join(root, "should-not-run")
	if _, err := Run(ctx, root, `printf bad > "$MARKER"`, append(os.Environ(), "MARKER="+marker)); !errors.Is(err, ErrLimit) {
		t.Fatalf("full execution slots did not refuse: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capacity refusal ran the command", err)
	}
	if err := held[0].Close(); err != nil {
		t.Fatal(err)
	}
	held = held[1:]
	out, err := Run(ctx, root, "printf ready", os.Environ())
	if err != nil || string(out) != "ready" {
		t.Fatalf("released slot not reusable: %q %v", out, err)
	}
}

func TestNotifierFirstKeepsOrdinarySessionUseAvailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	out, err := Run(context.Background(), root, "printf ready", os.Environ())
	if err != nil || string(out) != "ready" {
		t.Fatalf("first notifier: %q %v", out, err)
	}
	store := session.Store{Dir: root}
	if err := store.Put(session.Record{Agent: "claude", ID: "thread", Main: "idle"}); err != nil {
		t.Fatal("notifier poisoned fresh state admission", err)
	}
	if records, err := store.All(); err != nil || len(records) != 1 {
		t.Fatalf("session use after notification: %+v %v", records, err)
	}
}
