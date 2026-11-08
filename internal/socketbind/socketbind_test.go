package socketbind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == ChildFlag && os.Args[2] == "stalled.sock" {
		if path := os.Getenv("TMAG_TEST_BIND_STALL_PID"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
			time.Sleep(time.Hour)
			os.Exit(1)
		}
	}
	if handled, err := RunChild(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Helpers are intentionally short-lived even in race-instrumented test binaries.
	if err := os.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestBindUsesPinnedDirectoryAcrossAncestorReplacement(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "req")
	moved := original + "-moved"
	foreign := t.TempDir()
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, original); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bound, err := Listen(ctx, dir, "bound.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Listener.Close()
	if got, err := os.Getwd(); err != nil || got != cwd {
		t.Fatalf("parent cwd changed: %q %v", got, err)
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatalf("foreign target mutated: %d %v", len(entries), err)
	}
	path := filepath.Join(moved, "bound.sock")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if int64(sys.Dev) != bound.Device || sys.Ino != bound.Inode {
		t.Fatal("helper identity does not match pinned entry")
	}
	connected, err := Dial(ctx, dir, "bound.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer connected.Conn.Close()
	if err := bound.Listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	server, err := bound.Listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
}

func TestCanceledBindReapsOwnedChild(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	pidfile := filepath.Join(t.TempDir(), "child-pid")
	t.Setenv("TMAG_TEST_BIND_STALL_PID", pidfile)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := Listen(ctx, dir, "stalled.sock"); err == nil {
		t.Fatal("stalled helper succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("owned child cancellation took %s", elapsed)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned helper %d was not reaped: %v", pid, err)
	}
}

func TestExistingSocketTargetIsNeverUnlinked(t *testing.T) {
	root := t.TempDir()
	d, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	path := filepath.Join(root, "same.sock")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Listen(ctx, d, "same.sock"); err == nil {
		t.Fatal("overwrote an existing endpoint")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "preserve" {
		t.Fatalf("existing object changed: %q %v", b, err)
	}
}
