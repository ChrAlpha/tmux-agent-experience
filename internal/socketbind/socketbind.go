// Package socketbind confines Unix bind/connect to a pinned directory in a short-lived
// child. The child alone changes cwd and passes the socket over an inherited
// Unix socket. It has no daemon lifetime and stores no content.
package socketbind

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ChildFlag is an internal executable entry, handled before CLI/state setup.
// Embedding executables that use Listen must dispatch RunChild at their entry.
const ChildFlag = "--internal-socket-bind"

// Timeout bounds a helper's own work once it runs: the bind or connect and
// handing its descriptor over. Every Listen and Dial starts one, so a caller's
// budget for a single connection has to cover a process start as well.
const Timeout = time.Second

type Bound struct {
	Listener *net.UnixListener
	Device   int64
	Inode    uint64
}

// Listen inherits only the pinned directory, private IPC socket and descriptors
// whose exclusion must survive the parent's death until the child finishes.
// keep contains permanent lock descriptors borrowed from the caller.
func Listen(ctx context.Context, dir *os.File, name string, keep ...*os.File) (Bound, error) {
	file, frame, err := exchange(ctx, dir, name, "listen", keep...)
	if err != nil {
		return Bound{}, err
	}
	defer file.Close()
	if frame[0] != 1 {
		return Bound{}, errors.New("invalid listener bind response")
	}
	ln, err := net.FileListener(file)
	if err != nil {
		return Bound{}, err
	}
	own, ok := ln.(*net.UnixListener)
	if !ok {
		ln.Close()
		return Bound{}, errors.New("helper returned a non-Unix listener")
	}
	own.SetUnlinkOnClose(false)
	if err := ctx.Err(); err != nil {
		own.Close()
		return Bound{}, err
	}
	return Bound{Listener: own, Device: int64(binary.LittleEndian.Uint64(frame[1:9])), Inode: binary.LittleEndian.Uint64(frame[9:17])}, nil
}

// Connected pins a client channel to the same directory scope as the listener.
type Connected struct {
	Conn   net.Conn
	Device int64
	Inode  uint64
}

func Dial(ctx context.Context, dir *os.File, name string) (Connected, error) {
	file, frame, err := exchange(ctx, dir, name, "dial")
	if err != nil {
		return Connected{}, err
	}
	defer file.Close()
	if frame[0] != 2 {
		return Connected{}, errors.New("invalid socket dial response")
	}
	conn, err := net.FileConn(file)
	if err != nil {
		return Connected{}, err
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return Connected{}, err
	}
	return Connected{Conn: conn, Device: int64(binary.LittleEndian.Uint64(frame[1:9])), Inode: binary.LittleEndian.Uint64(frame[9:17])}, nil
}

func exchange(ctx context.Context, dir *os.File, name, kind string, keep ...*os.File) (_ *os.File, frame [17]byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, frame, err
	}
	if !validName(name) || len(keep) > 3 {
		return nil, frame, errors.New("invalid socket bind request")
	}
	syscall.ForkLock.RLock()
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(pair[0])
		syscall.CloseOnExec(pair[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, frame, err
	}
	parent, child := os.NewFile(uintptr(pair[0]), "socket-bind-parent"), os.NewFile(uintptr(pair[1]), "socket-bind-child")
	defer parent.Close()
	defer child.Close()
	conn, err := net.FileConn(parent)
	if err != nil {
		return nil, frame, err
	}
	defer conn.Close()
	ipc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, frame, errors.New("socket bind IPC is not Unix")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = ipc.SetDeadline(deadline)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, frame, err
	}
	cmd := exec.CommandContext(ctx, exe, ChildFlag, name, fmt.Sprint(len(keep)), kind)
	cmd.ExtraFiles = append([]*os.File{dir, child}, keep...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 20 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return nil, frame, err
	}
	child.Close()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Cancel()
			_ = cmd.Wait()
		}
	}()
	stop := context.AfterFunc(ctx, func() { ipc.Close() })
	defer stop()
	fd, err := receive(ipc, frame[:])
	if err != nil {
		return nil, frame, err
	}
	var file *os.File
	if fd >= 0 {
		file = os.NewFile(uintptr(fd), "answer-socket")
	}
	success := false
	defer func() {
		if !success && file != nil {
			file.Close()
		}
	}()
	if _, err := ipc.Write([]byte{1}); err != nil {
		return nil, frame, err
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		return nil, frame, fmt.Errorf("socket bind helper: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, frame, err
	}
	if frame[0] == 3 {
		return nil, frame, syscall.Errno(binary.LittleEndian.Uint64(frame[1:9]))
	}
	if file == nil {
		return nil, frame, errors.New("missing socket descriptor")
	}
	success = true
	return file, frame, nil
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 160 && !strings.ContainsAny(name, "/\\\x00")
}

// receive marks every received descriptor close-on-exec while holding ForkLock
// only around a nonblocking recvmsg attempt. Context cancellation closes ipc;
// no reader goroutine or unrelated process can survive the operation.
func receive(conn *net.UnixConn, frame []byte) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	var result = -1
	var readErr error
	received := 0
	err = raw.Read(func(fd uintptr) bool {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		control := make([]byte, syscall.CmsgSpace(4*4))
		n, oobn, flags, _, err := syscall.Recvmsg(int(fd), frame, control, 0)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return false
		}
		if errors.Is(err, syscall.EINTR) {
			return false
		}
		if err != nil {
			readErr = err
			return true
		}
		messages, err := syscall.ParseSocketControlMessage(control[:oobn])
		var rights []int
		if err == nil {
			for _, m := range messages {
				fds, e := syscall.ParseUnixRights(&m)
				if e != nil {
					err = e
					break
				}
				rights = append(rights, fds...)
			}
		}
		for _, v := range rights {
			syscall.CloseOnExec(v)
		}
		validRights := len(rights) == 1 || (len(rights) == 0 && frame[0] == 3)
		if err != nil || n < 1 || n > len(frame) || flags&syscall.MSG_CTRUNC != 0 || !validRights {
			for _, v := range rights {
				syscall.Close(v)
			}
			readErr = errors.New("incomplete socket bind descriptor response")
			return true
		}
		if len(rights) == 1 {
			result = rights[0]
		}
		received = n
		return true
	})
	if err != nil {
		if result >= 0 {
			syscall.Close(result)
		}
		return -1, err
	}
	if readErr == nil && received < len(frame) {
		if _, err := io.ReadFull(conn, frame[received:]); err != nil {
			if result >= 0 {
				syscall.Close(result)
			}
			return -1, err
		}
	}
	return result, readErr
}
