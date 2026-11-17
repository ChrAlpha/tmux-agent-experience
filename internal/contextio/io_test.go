package contextio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOwnedPipeCancellation(t *testing.T) {
	for _, writing := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[writing], func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			f := r
			if writing {
				f = w
			}
			// Match an inherited blocking descriptor, rather than relying on
			// the pollability of os.Pipe's original File object.
			fd := f.Fd()
			if err := syscall.SetNonblock(int(fd), false); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			owned, err := Own(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			defer owned.Close()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				if writing {
					_, err := owned.Write(make([]byte, 8<<20))
					done <- err
				} else {
					_, err := ReadAtMost(owned, 100)
					done <- err
				}
			}()
			<-started
			if writing {
				// At least one byte was written; the much larger frame cannot
				// finish without this reader's cooperation.
				one := make([]byte, 1)
				if _, err := io.ReadFull(r, one); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("I/O = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("owned pipe still blocked after cancellation")
			}
			if _, err := owned.Write([]byte("late")); !errors.Is(err, context.Canceled) {
				t.Fatalf("late output = %v", err)
			}
		})
	}
}

func fileFlags(t *testing.T, f *os.File) int {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var operation error
	if err := raw.Control(func(fd uintptr) {
		value, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		flags = int(value)
		if errno != 0 {
			operation = errno
		}
	}); err != nil {
		t.Fatal(err)
	}
	if operation != nil {
		t.Fatal(operation)
	}
	return flags
}

func TestOwnedClosePreservesOriginalPollablePipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	before := fileFlags(t, r)
	if before&syscall.O_NONBLOCK == 0 {
		t.Fatal("fixture is not originally pollable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owned, err := Own(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if got := fileFlags(t, r); got != before {
		t.Fatalf("Own changed original flags: %x -> %x", before, got)
	}
	cancel()
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fileFlags(t, r); got != before {
		t.Fatalf("Close changed original flags: %x -> %x", before, got)
	}
	// Fd() can retain a misleading deadline-capable interface while changing
	// the Go poller's isBlocking bit. Verify a real pending read ends by its
	// deadline, then prove the same descriptor still transfers bytes.
	if err := r.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { var one [1]byte; _, err := r.Read(one[:]); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("original cancellation = %v", err)
		}
	case <-time.After(time.Second):
		// Closing w also releases a wrongly blocking read before failing.
		_ = w.Close()
		readErr := <-done
		t.Fatalf("original poller lost its deadline behavior (released read: %v)", readErr)
	}
	if err := r.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "reused"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("reused"))
	if _, err := io.ReadFull(r, got); err != nil || string(got) != "reused" {
		t.Fatalf("original reuse=%q err=%v", got, err)
	}
}

func TestReadFileIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(context.Background(), path, 4); err == nil {
		t.Fatal("unbounded metadata read")
	}
	if got, err := ReadFile(context.Background(), path, 5); err != nil || string(got) != "12345" {
		t.Fatalf("ReadFile at its limit = %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadFile(ctx, path, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadFile after cancellation: %v", err)
	}
}

// Lines cross chunk boundaries and outgrow a chunk; a line still being
// written comes as it is, and one longer than the bound comes as nil.
func TestBackward(t *testing.T) {
	long := strings.Repeat("x", 3*backwardChunk)
	over := strings.Repeat("z", 5*backwardChunk)
	lines := []string{"a", long, "", "b", over, strings.Repeat("y", backwardChunk-1), "c"}
	data := []byte(strings.Join(lines, "\n") + "\n{partial")
	type line struct {
		text  string
		nil   bool
		start int
	}
	var got []line
	err := Backward(context.Background(), bytes.NewReader(data), int64(len(data)), int64(4*backwardChunk), func(b []byte, start int64) bool {
		got = append(got, line{string(b), b == nil, int(start)})
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	all := append(lines, "{partial")
	var want []line
	at := len(data)
	for i := len(all) - 1; i >= 0; i-- {
		at -= len(all[i])
		if all[i] == over {
			want = append(want, line{"", true, at})
		} else {
			want = append(want, line{all[i], false, at})
		}
		at-- // the newline before it
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q nil=%v at %d, want %.10q nil=%v at %d", i, trim(got[i].text), got[i].nil, got[i].start, want[i].text, want[i].nil, want[i].start)
		}
	}
	got = nil
	if err := Backward(context.Background(), bytes.NewReader([]byte("a\nb\n")), 4, 16, func(b []byte, start int64) bool {
		got = append(got, line{string(b), b == nil, int(start)})
		return string(b) == "b"
	}); err != nil || len(got) != 2 || got[0] != (line{"", false, 4}) || got[1] != (line{"b", false, 2}) {
		t.Fatalf("stopping early: %+v, %v", got, err)
	}
}

func trim(s string) string {
	if len(s) > 10 {
		return s[:10] + "…"
	}
	return s
}
