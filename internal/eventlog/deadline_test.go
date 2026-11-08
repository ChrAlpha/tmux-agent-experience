package eventlog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Run the actual reusable APIs in a sacrificial subprocess. A blocking open
// regression must fail promptly rather than strand a worker in the test runner.
func TestSpecialMetadataDoesNotBlockContext(t *testing.T) {
	if operation := os.Getenv("TMAG_LOG_BOUNDARY_TEST"); operation != "" {
		l := Open(os.Getenv("TMAG_LOG_BOUNDARY_DIR"))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		var err error
		switch operation {
		case "head":
			_, err = l.Head(ctx)
		case "replay":
			_, err = l.Replay(ctx, Cursor{})
		case "append":
			err = l.Append(ctx, &Event{Event: "Stop"})
		case "scan":
			_, _, _, err = l.scan(ctx, currentName, 0, 0, 0, true)
		case "replace":
			_, err = l.replacePrefix(ctx, currentName, segment{})
		}
		cancel()
		if err == nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		operation, name string
		repair          bool
	}{
		{"head", historyName, false}, {"replay", historyName, false},
		{"head", seqName, false}, {"replay", seqName, false},
		{"head", currentName, false}, {"replay", rotatedName, false},
		{"append", seqName + ".tmp", false}, {"append", historyName + ".tmp", false},
		{"head", currentName + ".repair.tmp", true},
		{"scan", currentName, false}, {"replace", currentName, false},
	} {
		t.Run(fixture.operation+"/"+fixture.name, func(t *testing.T) {
			l := Open(t.TempDir())
			appendN(t, l, 1)
			if fixture.repair {
				appendRaw(t, l, []byte(`{"unfinished":`))
			}
			path := l.path(fixture.name)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestSpecialMetadataDoesNotBlockContext$")
			cmd.Env = append(os.Environ(), "TMAG_LOG_BOUNDARY_TEST="+fixture.operation, "TMAG_LOG_BOUNDARY_DIR="+l.Dir,
				"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("metadata boundary blocked or succeeded: %v %s", err, out)
			}
			if ctx.Err() != nil {
				t.Fatal("operation did not return within its context")
			}
		})
	}
}

func TestCanceledPublicationCannotReplaceCheckpoint(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	before, err := os.ReadFile(l.path(seqName))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.writeSeq(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := l.saveHistory(ctx, history{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(l.path(seqName))
	if err != nil || string(before) != string(after) {
		t.Fatalf("canceled publication replaced sequence: %v", err)
	}
	if _, err := os.Stat(l.path(seqName + ".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled publication created temporary: %v", err)
	}
}

func TestCheckpointTemporaryCannotFollowAlias(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink", true: "hard link"}[hard], func(t *testing.T) {
			l := Open(t.TempDir())
			sentinel := filepath.Join(l.Dir, "sentinel")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if hard {
				link = os.Link
			}
			if err := link(sentinel, l.path(seqName+".tmp")); err != nil {
				t.Fatal(err)
			}
			if err := l.writeSeq(context.Background(), 123); err == nil {
				t.Fatal("published through an aliased temporary")
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
				t.Fatalf("alias target modified: %q %v", got, err)
			}
		})
	}
}
