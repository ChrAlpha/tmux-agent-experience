package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestDoctorReportsCurrentFaultContactAndIncompleteSample(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a := app{log: eventlog.Open(root)}
	contact := time.Unix(20, 123).UTC()
	if err := a.store().Put(session.Record{Agent: "claude", ID: "thread", Main: "idle", LastHookAt: contact}); err != nil {
		t.Fatal(err)
	}
	if got := a.checkRecords(ctx); got.Status != "ok" || !strings.Contains(got.Detail, contact.Format(time.RFC3339Nano)) {
		t.Fatalf("missing hook contact: %+v", got)
	}
	bad := filepath.Join(root, "sessions", "claude", "broken.json")
	if err := os.WriteFile(bad, []byte("private decoder sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.checkRecords(ctx); got.Status != "fail" || !strings.Contains(got.Detail, "claude/broken") || !strings.Contains(got.Detail, "decode") || strings.Contains(got.Detail, "sentinel") {
		t.Fatalf("fault not actionable/private: %+v", got)
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hook.log"), []byte(time.Now().UTC().Format(time.RFC3339)+" notify private legacy stderr sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.checkHookErrors(ctx); strings.Contains(got.Detail, "sentinel") || !strings.Contains(got.Detail, "redacted") {
		t.Fatalf("legacy log content escaped: %+v", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := a.checkRecords(canceled); got.Status != "warn" || !strings.Contains(got.Detail, "incomplete") {
		t.Fatalf("cancellation became empty healthy result: %+v", got)
	}
}
func TestHostProbeContainsHangingCommandAndRetainedPipe(t *testing.T) {
	for _, script := range []string{"exec /bin/sleep 60", "/bin/sleep 60 & exit 0"} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		started := time.Now()
		out, err := run(ctx, "/bin/sh", "-c", script)
		cancel()
		if err == nil || out != "" || time.Since(started) > time.Second {
			t.Fatalf("unbounded/partial probe: elapsed=%s err=%v", time.Since(started), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := run(ctx, "/bin/sh", "-c", "exit 0"); !errors.Is(err, context.Canceled) {
		t.Fatal("probe lost parent cancellation", err)
	}
	cap := &hostOutput{}
	if n, err := cap.Write(bytes.Repeat([]byte{'x'}, (1<<20)+1)); err != nil || n != (1<<20)+1 || cap.Len() != 1<<20 || !cap.truncated {
		t.Fatal("retained host output exceeded cap")
	}
}
