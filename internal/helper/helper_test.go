package helper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHelperOwnsDescendantsAndRetainedPipes(t *testing.T) {
	for _, earlyExit := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled group", true: "retained pipe after exit"}[earlyExit], func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "late-mutation")
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			script := `(sleep 0.5; printf late > "$TMAG_HELPER_MARKER") & wait`
			if earlyExit {
				script = `(sleep 0.5; printf late > "$TMAG_HELPER_MARKER") & exit 0`
			}
			cmd, cleanup := Command(ctx, "/bin/sh", "-c", script)
			defer cleanup()
			cmd.Env = append(os.Environ(), "TMAG_HELPER_MARKER="+marker)
			started := time.Now()
			_, err := cmd.Output()
			cleanup()
			if earlyExit {
				if !errors.Is(err, exec.ErrWaitDelay) {
					t.Fatalf("retained pipe = %v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("canceled helper = %v", err)
			}
			if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
				t.Fatalf("helper cleanup outlived budget: %s", elapsed)
			}
			if cmd.ProcessState == nil {
				t.Fatal("direct helper was not reaped")
			}
			// Give the descendant's attempted side effect time to occur. The
			// helper group has already been killed; this is a negative oracle.
			timer := time.NewTimer(600 * time.Millisecond)
			defer timer.Stop()
			<-timer.C
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("descendant mutated after return: %v", err)
			}
		})
	}
}

func TestAlreadyCanceledHelperDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "started")
	cmd, cleanup := Command(ctx, "/bin/sh", "-c", `printf started > "$TMAG_HELPER_MARKER"`)
	defer cleanup()
	cmd.Env = append(os.Environ(), "TMAG_HELPER_MARKER="+marker)
	if err := cmd.Run(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled helper started: %v", err)
	}
}
