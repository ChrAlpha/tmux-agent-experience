package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Fixtures replace PATH and all host/tmux locations. They never start a real
// agent or read the person's host configuration.
func hostProbeFixture(t *testing.T, cli, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ready := filepath.Join(dir, "ready")
	t.Setenv("TMAG_HOST_PROBE_READY", ready)
	if err := os.WriteFile(filepath.Join(dir, cli), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return ready
}
func hostProbeResult(t *testing.T, ctx context.Context, probe func(context.Context) host) host {
	t.Helper()
	done := make(chan host, 1)
	go func() { done <- probe(ctx) }()
	select {
	case h := <-done:
		return h
	case <-time.After(2 * time.Second):
		t.Fatal("host probe escaped diagnostic parent or retained a descendant's output")
	}
	return host{}
}
func fixturePID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("helper did not reach the tested boundary", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatal("invalid fixture pid", err)
	}
	return pid
}
func assertFixtureExited(t *testing.T, pid int) {
	t.Helper()
	// Zombies no longer execute or hold descriptors. SnapshotContext may be
	// incomplete because unrelated container PIDs are unreadable, so inspect only
	// this owned fixture rather than requiring a globally complete process table.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			_, tail, ok := strings.Cut(string(data), ") ")
			if ok && strings.HasPrefix(tail, "Z ") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("owned helper %d still running after probe returned", pid)
}
func TestHostProbesRespectDiagnosticParentAndContainOutputDescendants(t *testing.T) {
	for _, cli := range []string{"claude", "codex"} {
		for _, phase := range []string{"version", "plugins", "descendant output"} {
			t.Run(cli+"/"+phase, func(t *testing.T) {
				body := `if [ "$1" = "--version" ]; then
 printf '%s\n' 'agent fixture 1.0'
 exit 0
fi
printf '%s\n' "$$" > "$TMAG_HOST_PROBE_READY"
exec /bin/sleep 60
`
				switch phase {
				case "version":
					body = "printf '%s\\n' \"$$\" > \"$TMAG_HOST_PROBE_READY\"\nexec /bin/sleep 60\n"
				case "descendant output":
					body = `if [ "$1" = "--version" ]; then
 printf '%s\n' 'agent fixture 1.0'
 exit 0
fi
/bin/sleep 60 &
printf '%s\n' "$!" > "$TMAG_HOST_PROBE_READY"
exit 0
`
				}
				ready := hostProbeFixture(t, cli, body)
				probe := claudeHostContext
				if cli == "codex" {
					probe = codexHostContext
				}
				ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
				defer cancel()
				start := time.Now()
				h := hostProbeResult(t, ctx, probe)
				if time.Since(start) > time.Second || !h.Present || h.Listed || h.ProbeFailure == "" {
					t.Fatalf("hung probe was absent/success: %+v", h)
				}
				pid := fixturePID(t, ready)
				assertFixtureExited(t, pid)
				c := claudeCheck(h, true)
				if cli == "codex" {
					c, _ = codexCheck(ctx, h, true)
				}
				if c.Status != "warn" || strings.Contains(c.Detail, "not installed") || strings.Contains(c.Detail, "run tmag setup") {
					t.Fatalf("incomplete check suggested missing-plugin action: %+v", c)
				}
			})
		}
	}
}
func TestHostProbeCapsRetainedOutputBeforeParsing(t *testing.T) {
	hostProbeFixture(t, "claude", `if [ "$1" = "--version" ]; then
 printf '%s\n' 'agent fixture 1.0'
 exit 0
fi
# A fixed private file avoids a slow per-byte shell loop.
/bin/cat "$TMAG_HOST_PROBE_FLOOD"
`)
	flood := filepath.Join(t.TempDir(), "flood")
	if err := os.WriteFile(flood, []byte(strings.Repeat("x", hostOutputLimit+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMAG_HOST_PROBE_FLOOD", flood)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h := claudeHostContext(ctx)
	if h.ProbeFailure != "output_capacity" || h.Listed || !h.Present {
		t.Fatalf("oversized output was parsed or treated absent: %+v", h)
	}
	out := &probeOutput{limit: 7}
	if n, err := out.Write([]byte("123456789")); err != nil || n != 9 || len(out.data) != 7 || !out.overflow {
		t.Fatalf("retention cap broken: %+v %d %v", out, n, err)
	}
}
func TestCanceledHostAndFinalDaemonProbeAreUnknown(t *testing.T) {
	hostProbeFixture(t, "codex", "exit 99\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := codexHostContext(ctx)
	if h.ProbeFailure != "timeout_or_canceled" || h.Present {
		t.Fatalf("canceled discovery claimed installed/absent: %+v", h)
	}
	if _, _, err := codexDaemonContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("daemon scan lost diagnostic parent: %v", err)
	}
	if _, err := proc.EnvironContext(ctx, os.Getpid(), "PATH"); !errors.Is(err, context.Canceled) {
		t.Fatalf("daemon environment probe lost parent: %v", err)
	}
	c, complete := codexCheck(ctx, host{Present: true, Listed: true, HookApproval: true}, true)
	if complete || c.Status != "warn" || strings.Contains(c.Detail, "not installed") || strings.Contains(c.Detail, "run tmag setup") {
		t.Fatalf("final diagnostic cancellation fabricated advice: %+v", c)
	}
}

func TestDoctorSharesOneBudgetAcrossHostProbes(t *testing.T) {
	a, out := doctorFixture(t)
	ready := hostProbeFixture(t, "claude", "printf '%s\\n' \"$$\" > \"$TMAG_HOST_PROBE_READY\"\nexec /bin/sleep 60\n")
	dir := filepath.Dir(ready)
	// Discovery has no private server sockets. The version helper is the only
	// tmux invocation, and both agent executables are private fixture scripts.
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' 'tmux 3.3a'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "codex-started")
	t.Setenv("TMAG_HOST_PROBE_SECOND", second)
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nprintf started > \"$TMAG_HOST_PROBE_SECOND\"\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	a.ctx = ctx
	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- a.cmdDoctor([]string{"--json"}) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("doctor gave host probes independent deadlines")
	}
	if time.Since(start) > doctorProbeBudget+500*time.Millisecond {
		t.Fatalf("shared diagnostic budget escaped: %s", time.Since(start))
	}
	assertFixtureExited(t, fixturePID(t, ready))
	if _, err := os.Lstat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("second host was launched after shared budget expired", err)
	}
	var doc struct {
		Checks      []check             `json:"checks"`
		Operational OperationalEvidence `json:"operational"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Operational.Complete {
		t.Fatal("partial diagnostic pass claimed complete")
	}
	for _, name := range []string{"claude", "codex"} {
		c := namedCheck(doc.Checks, name)
		if c.Status != "warn" || strings.Contains(c.Detail, "not installed") || strings.Contains(c.Detail, "run tmag setup") {
			t.Fatalf("deadline fabricated host advice: %+v", c)
		}
	}
}
