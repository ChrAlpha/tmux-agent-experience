package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCanceledSnapshotReturnsNoTable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if table, err := SnapshotContext(ctx); table != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("table=%v err=%v", table, err)
	}
}

func TestUnreadablePIDPreservesLivenessWithoutAuthority(t *testing.T) {
	table := NewTable([]Process{{PID: 123, Uncertain: true, Comm: "codex", Args: "codex app-server"}})
	if !table.Alive(123, "previous-start") {
		t.Fatal("unreadable PID treated as death")
	}
	if _, _, ok := table.FindAgent(123); ok {
		t.Fatal("unreadable PID authorized an attachment")
	}
	if _, ok := table.HostOf(123); ok {
		t.Fatal("unreadable PID authorized a caller")
	}
}

func TestPartialPSOutputCannotAuthorizeReconciliation(t *testing.T) {
	dir := t.TempDir()
	shim := "#!/bin/sh\nprintf '%s\\n' '1 0 1 1 Sun Nov 8 09:00:00 2026 init' 'broken'\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if records, err := readPSContext(context.Background()); records != nil || err == nil {
		t.Fatalf("partial process table=%v err=%v", records, err)
	}
}

func TestRealmDiscoveryHonorsCanceledParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if realm, err := RealmContext(ctx); realm != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("realm=%q err=%v", realm, err)
	}
}
