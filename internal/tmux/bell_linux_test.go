package tmux

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY returns a pseudo-terminal's master and the path of its slave.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal(e)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n)
}

func TestBellNeverWaitsForTheTerminal(t *testing.T) {
	_, slave := openPTY(t)
	f, err := os.OpenFile(slave, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Stop the terminal's output, as Ctrl-S does: a blocking write would hang.
	const tcxonc, tcooff = 0x540A, 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tcxonc, tcooff); e != 0 {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- Pane{ID: "%1", TTY: slave}.Bell() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("bell reported success on a stopped terminal")
		}
	case <-time.After(time.Second):
		t.Fatal("bell blocked on a stopped terminal")
	}
}

func TestBellReachesTheTerminal(t *testing.T) {
	master, slave := openPTY(t)
	if err := (Pane{ID: "%1", TTY: slave}).Bell(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_ = master.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := master.Read(buf); err != nil || n != 1 || buf[0] != '\a' {
		t.Fatalf("terminal read %q, %v", buf[:n], err)
	}
}
