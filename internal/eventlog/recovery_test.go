package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func appendRaw(t *testing.T, l *Log, data []byte) {
	t.Helper()
	err := l.WithLock(bg, func() error {
		f, err := os.OpenFile(l.path(currentName), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFollowReportsLostInitialHistoryBeforeCallback(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 1
	appendN(t, l, 1)
	old, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 4)
	err = l.Follow(bg, old.Seq, time.Millisecond, func(Event) error {
		t.Fatal("delivered a retained suffix without its missing history")
		return nil
	})
	if !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("Follow = %v", err)
	}
}

func TestSlowFollowReportsMultipleRotationLoss(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 1
	appendN(t, l, 1)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		calls := 0
		done <- l.FollowCursor(ctx, c, time.Millisecond, func(Event) error {
			calls++
			if calls > 1 {
				return errors.New("delivered an event beyond missing history")
			}
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("follower never entered callback")
	}
	// Capture has released the log lock. Rotate several times while the
	// callback is deliberately blocked, rather than hoping scheduling lags.
	for range 6 {
		if err := l.Append(ctx, &Event{Event: "Stop", State: "idle"}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrHistoryUnavailable) {
			t.Fatalf("FollowCursor = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("follower failed to report history loss")
	}
}

func TestAllocationGapsAtRetentionBoundaryRemainReplayable(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 1
	appendN(t, l, 1)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.writeSeq(bg, c.Seq+100); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 2)
	// The retained suffix begins 101 numbers after c, but the only
	// discarded committed event was c itself. No transition is missing.
	b, err := l.Replay(bg, Cursor{Seq: c.Seq})
	if err != nil || len(b.Events) != 2 {
		t.Fatalf("Replay across allocation holes = %+v, %v", b, err)
	}
	if b.Events[0].Seq != c.Seq+101 {
		t.Fatalf("first retained sequence = %d", b.Events[0].Seq)
	}
}

func TestPartialTailRepairReplacesInodeAndFencesExistingCursors(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 2)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	oldFile, err := os.Open(l.path(currentName))
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	oldInfo, err := oldFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// Emulate a writer dying after reservation and halfway through its line.
	if err := l.writeSeq(bg, c.Seq+1); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, l, []byte(`{"seq":`))
	appendN(t, l, 2)
	newInfo, err := os.Stat(l.path(currentName))
	if err != nil || os.SameFile(oldInfo, newInfo) {
		t.Fatalf("repair kept old inode: %v", err)
	}
	if _, err := l.Replay(bg, c); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("pre-repair token accepted: %v", err)
	}
	if _, complete, err := l.Since(bg, c.Seq); err != nil || complete {
		t.Fatalf("pre-repair numeric cursor: complete=%v, err=%v", complete, err)
	}
	events, _, err := l.Since(bg, 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("repaired records = %d, %v", len(events), err)
	}
	if events[2].Seq <= c.Seq+1 || events[3].Seq <= events[2].Seq {
		t.Fatal("repair reused a reserved sequence")
	}
	// A fresh checkpoint continues normally after the invalidation boundary.
	fresh, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)
	if b, err := l.Replay(bg, fresh); err != nil || len(b.Events) != 1 {
		t.Fatalf("post-repair continuation = %+v, %v", b, err)
	}
}

func TestCompleteMalformedRecordBlocksAppendAndReplay(t *testing.T) {
	for _, suffix := range []string{"broken\n", "broken\n{\"seq\":1}\n"} {
		t.Run(suffix, func(t *testing.T) {
			l := Open(t.TempDir())
			appendN(t, l, 1)
			c, err := l.Head(bg)
			if err != nil {
				t.Fatal(err)
			}
			appendRaw(t, l, []byte(suffix))
			before, err := os.ReadFile(l.path(currentName))
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Append(bg, &Event{Event: "Stop"}); !errors.Is(err, ErrIntegrityLoss) {
				t.Fatalf("Append hid corruption: %v", err)
			}
			if _, err := l.Replay(bg, c); !errors.Is(err, ErrIntegrityLoss) {
				t.Fatalf("Replay hid corruption: %v", err)
			}
			after, err := os.ReadFile(l.path(currentName))
			if err != nil || string(before) != string(after) {
				t.Fatal("interior corruption was truncated or appended to")
			}
		})
	}
}

func TestFailedDestructiveOperationLeavesConservativeRecoveryFence(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 1
	appendN(t, l, 2)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	// Emulate death after publishing the rotation intent and retention fence
	// but before rename. Extra invalidation is safe; optimistic continuity is not.
	err = l.WithLock(bg, func() error {
		h, err := l.history(bg)
		if err != nil {
			return err
		}
		h.Floor, h.Dirty = c.Seq, true
		return l.saveHistory(bg, h)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Replay(bg, Cursor{Seq: c.Seq - 1}); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("intent fence forgotten after restart: %v", err)
	}
	if _, err := l.Replay(bg, c); err != nil {
		t.Fatalf("cursor at the retained boundary rejected: %v", err)
	}
}

func TestCheckpointPreservesHighWaterWithoutSequenceFile(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.writeSeq(bg, c.Seq+100); err != nil {
		t.Fatal(err)
	}
	reserved, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(l.path(seqName)); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)
	if seq, err := l.Seq(); err != nil || seq != reserved.Seq+1 {
		t.Fatalf("high-water reused: %d, %v", seq, err)
	}
}

func TestLegacyAppendAndRotationKeepValidNumericReplay(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	legacy := Event{Seq: c.Seq + 50, Event: "Stop", State: "idle"}
	line, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, l, append(line, '\n'))
	if err := os.Rename(l.path(currentName), l.path(rotatedName)); err != nil {
		t.Fatal(err)
	}
	legacy.Seq++
	line, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.path(currentName), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.writeSeq(bg, legacy.Seq); err != nil {
		t.Fatal(err)
	}
	events, complete, err := l.Since(bg, c.Seq)
	if err != nil || !complete || len(events) != 2 {
		t.Fatalf("legacy continuation = %d, %v, %v", len(events), complete, err)
	}
}

func TestInternalCursorRejectsRecreatedEmptyDirectory(t *testing.T) {
	l := Open(t.TempDir())
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(l.Dir, historyName)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Replay(bg, c); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("accepted token from another incarnation: %v", err)
	}
}

func TestCanceledLogOperationNeverEntersCallback(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "unused"))
	ctx, cancel := context.WithCancel(bg)
	cancel()
	err := l.WithLock(ctx, func() error {
		t.Fatal("entered callback with canceled context and uncontended lock")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithLock = %v", err)
	}
	if _, err := os.Stat(l.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled operation touched state directory: %v", err)
	}
}

func TestAppendRecoversActualShortWrite(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAppendShortWriteHelper$")
	cmd.Env = append(os.Environ(), "TMAG_TEST_SHORT_WRITE_DIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("short-write subprocess: %v\n%s", err, out)
	}
}

// The file-size limit and signal disposition belong only to this subprocess.
// This forces the production File.Write to partially write a real record.
func TestAppendShortWriteHelper(t *testing.T) {
	dir := os.Getenv("TMAG_TEST_SHORT_WRITE_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	l := Open(dir)
	appendN(t, l, 10)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(l.path(currentName))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(l.path(currentName))
	if err != nil {
		t.Fatal(err)
	}
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	limited := original
	limited.Cur = uint64(st.Size() + 17)
	// XNU clips against the descriptor offset before the filesystem applies
	// O_APPEND; a newly opened descriptor starts at zero. Make the record
	// exceed the entire limit, not just the 17 bytes left after the old EOF.
	// Linux instead checks the limit against the append position at EOF.
	e := Event{Event: "Stop", State: "idle", Cwd: strings.Repeat("x", int(limited.Cur)+1)}
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	writeErr := l.Append(bg, &e)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(writeErr, io.ErrShortWrite) && !errors.Is(writeErr, syscall.EFBIG) {
		t.Fatalf("append did not report the file-size-limited write: %v", writeErr)
	}
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	damaged, err := os.ReadFile(l.path(currentName))
	if err != nil {
		t.Fatal(err)
	}
	written := len(damaged) - len(before)
	if written <= 0 || written >= len(line) || !bytes.HasPrefix(damaged, before) || !bytes.Equal(damaged[len(before):], line[:written]) {
		t.Fatalf("did not append a strict record prefix: wrote %d of %d bytes, error %v", written, len(line), writeErr)
	}
	if _, err := l.Replay(bg, c); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("short write did not invalidate old continuation: %v", err)
	}
	appendN(t, l, 1)
	events, _, err := l.Since(bg, 0)
	if err != nil || len(events) != 11 || events[10].Seq <= e.Seq {
		t.Fatalf("short-write recovery: %d records, failed seq %d, err %v", len(events), e.Seq, err)
	}
}

func TestRotationRecoveryAtEveryPublicationBoundary(t *testing.T) {
	for _, phase := range []string{"intent", "renamed", "checkpointed"} {
		t.Run(phase, func(t *testing.T) {
			l := Open(t.TempDir())
			l.MaxBytes = 1
			appendN(t, l, 2)
			c, err := l.Head(bg)
			if err != nil {
				t.Fatal(err)
			}
			err = l.WithLock(bg, func() error {
				h, err := l.history(bg)
				if err != nil {
					return err
				}
				h.Floor, h.Dirty = h.Rotated.Last, true
				if err := l.saveHistory(bg, h); err != nil || phase == "intent" {
					return err
				}
				if err := os.Rename(l.path(currentName), l.path(rotatedName)); err != nil {
					return err
				}
				if phase == "renamed" {
					return nil
				}
				h.Rotated, h.Current, h.Dirty = h.Current, segment{}, false
				return l.saveHistory(bg, h)
			})
			if err != nil {
				t.Fatal(err)
			}
			// c follows both files: discarding older records loses nothing
			// after it. Recovery must not demand an epoch for numeric replay.
			b, err := l.Replay(bg, Cursor{Seq: c.Seq})
			if err != nil || len(b.Events) != 0 {
				t.Fatalf("recovery = %+v, %v", b, err)
			}
			appendN(t, l, 1)
			b, err = l.Replay(bg, b.Cursor)
			if err != nil || len(b.Events) != 1 {
				t.Fatalf("post-recovery continuation = %+v, %v", b, err)
			}
		})
	}
}

func TestRemovedRotatedSegmentInvalidatesOnlyLostHistory(t *testing.T) {
	l := Open(t.TempDir())
	l.MaxBytes = 1
	appendN(t, l, 2)
	c, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(l.path(rotatedName)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Replay(bg, Cursor{Seq: c.Seq - 2}); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("lost rotated record was certified: %v", err)
	}
	if b, err := l.Replay(bg, Cursor{Seq: c.Seq - 1}); err != nil || len(b.Events) != 1 {
		t.Fatalf("retained numeric continuation = %+v, %v", b, err)
	}
}

func TestLegacyWriterFillingAnExposedReservationInvalidatesContinuation(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	committed, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.writeSeq(bg, committed.Seq+10); err != nil {
		t.Fatal(err)
	}
	exposed, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	// A baseline writer sees a missing seq and reconstructs it from the
	// last committed record, filling a number below the exposed checkpoint.
	if err := os.Remove(l.path(seqName)); err != nil {
		t.Fatal(err)
	}
	legacy := Event{Seq: committed.Seq + 1, Event: "Stop", State: "idle"}
	line, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, l, append(line, '\n'))
	if err := l.writeSeq(bg, legacy.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Replay(bg, exposed); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("filled reservation silently skipped: %v", err)
	}
	if _, complete, err := l.Since(bg, exposed.Seq); err != nil || complete {
		t.Fatalf("numeric filled reservation accepted: %v, %v", complete, err)
	}
}
