package eventlog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

func healthFixture(t *testing.T) *Log {
	t.Helper()
	l := Open(t.TempDir())
	// Normal session publication establishes this ownership manifest. The log
	// itself predates ownedfs and does not adopt a root on a diagnostic read.
	d, err := ownedfs.Open(context.Background(), l.Dir, "known", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(context.Background(), &Event{Agent: "codex", Session: "thread", Event: "SessionStart"}); err != nil {
		t.Fatal(err)
	}
	return l
}
func TestLogHealthPreservesCorruptionAndIncompleteTail(t *testing.T) {
	for _, tc := range []struct{ name, suffix, want string }{
		{"complete corruption", "private log sentinel\n", "integrity_loss"},
		{"incomplete write", `{"seq":`, "incomplete_tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			l := healthFixture(t)
			if got := l.ProbeHealth(ctx); got.State != "readable" || !got.Complete || got.Records != 1 {
				t.Fatalf("healthy log: %+v", got)
			}
			path := l.path(currentName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, tc.suffix...)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			historyBefore, err := os.ReadFile(l.path(historyName))
			if err != nil {
				t.Fatal(err)
			}
			got := l.ProbeHealth(ctx)
			if got.State != tc.want || got.Complete || got.File != currentName || got.Offset == 0 {
				t.Fatalf("corruption hidden: %+v", got)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, after) {
				t.Fatal("diagnosis changed evidence", err)
			}
			historyAfter, err := os.ReadFile(l.path(historyName))
			if err != nil || !bytes.Equal(historyBefore, historyAfter) {
				t.Fatal("diagnosis changed checkpoint", err)
			}
		})
	}
}
func TestLogHealthUnknownCapacityAndContention(t *testing.T) {
	ctx := context.Background()
	l := healthFixture(t)
	held, err := lockfile.AcquireWithin(ctx, l.Dir, lockName, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.ProbeHealth(ctx); got.State != "contended" || got.Complete {
		t.Fatalf("busy log claimed healthy: %+v", got)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.path(currentName), bytes.Repeat([]byte{'x'}, HealthByteLimit+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := l.ProbeHealth(ctx); got.State != "capacity" || got.Complete {
		t.Fatalf("large log claimed healthy: %+v", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := l.ProbeHealth(canceled); got.State != "canceled" || got.Complete {
		t.Fatalf("canceled log claimed healthy: %+v", got)
	}
	missing := Open(filepath.Join(t.TempDir(), "missing"))
	if got := missing.ProbeHealth(ctx); got.State != "never_recorded" {
		t.Fatalf("missing: %+v", got)
	}
	if _, err := os.Lstat(missing.Dir); !os.IsNotExist(err) {
		t.Fatal("diagnosis created state directory", err)
	}
}
