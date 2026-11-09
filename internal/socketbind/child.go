package socketbind

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// RunChild runs only in the explicitly spawned executable mode. It returns to
// main, which owns the exit code; reusable APIs and HTTP handlers never exit.
// No parent-process path calls fchdir. Descriptors 3/4 are directory/IPC; any
// remaining descriptors retain the creator's locks until acknowledgement/EOF.
func RunChild(args []string) (bool, error) {
	if len(args) == 0 || args[0] != ChildFlag {
		return false, nil
	}
	if len(args) != 4 || !validName(args[1]) || (args[3] != "listen" && args[3] != "dial") {
		return true, errors.New("invalid bind child arguments")
	}
	keep, err := strconv.Atoi(args[2])
	if err != nil || keep < 0 || keep > 3 {
		return true, errors.New("invalid bind descriptor count")
	}
	dir, channel := os.NewFile(3, "bind-directory"), os.NewFile(4, "bind-control")
	defer dir.Close()
	defer channel.Close()
	for i := range keep {
		defer os.NewFile(uintptr(5+i), "bind-exclusion").Close()
	}
	st, err := dir.Stat()
	if err != nil {
		return true, err
	}
	if !st.IsDir() {
		return true, errors.New("bind directory is not a directory")
	}
	conn, err := net.FileConn(channel)
	if err != nil {
		return true, err
	}
	defer conn.Close()
	ipc, ok := conn.(*net.UnixConn)
	if !ok {
		return true, errors.New("invalid bind control socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_ = ipc.SetDeadline(deadline)
	stopIPC := context.AfterFunc(ctx, func() { ipc.Close() })
	defer stopIPC()
	// Watch parent EOF from setup onward, including a blocked Unix connect.
	// Only this owned socket is read, and closing it cancels the real read.
	acknowledgement := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(ipc, b[:])
		if err == nil && b[0] != 1 {
			err = errors.New("invalid bind acknowledgement")
		}
		acknowledgement <- err
		if err != nil {
			cancel()
		}
	}()

	// fchdir operates on the inherited inode, even if every original pathname
	// component was renamed or replaced after the parent opened it.
	if err := syscall.Fchdir(int(dir.Fd())); err != nil {
		return true, err
	}
	name := args[1]
	if args[3] == "dial" {
		return true, dialChild(ctx, ipc, acknowledgement, name)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		return true, err
	}
	ln.SetUnlinkOnClose(false)
	defer ln.Close()
	_ = ln.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	before, err := os.Lstat(name)
	if err != nil {
		return true, err
	}
	if before.Mode()&os.ModeSocket == 0 {
		return true, errors.New("bind entry is not a socket")
	}
	// Prove that this literal entry reaches this listener, using only a nonce
	// kept in memory. No request tool/input or decision enters this protocol.
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return true, err
	}
	var dial net.Dialer
	probe, err := dial.DialContext(ctx, "unix", name)
	if err != nil {
		return true, err
	}
	defer probe.Close()
	stopProbe := context.AfterFunc(ctx, func() { probe.Close() })
	defer stopProbe()
	_ = probe.SetDeadline(deadline)
	if _, err := probe.Write(nonce[:]); err != nil {
		return true, err
	}
	accepted, err := ln.Accept()
	if err != nil {
		return true, err
	}
	defer accepted.Close()
	stopAccepted := context.AfterFunc(ctx, func() { accepted.Close() })
	defer stopAccepted()
	_ = accepted.SetDeadline(deadline)
	var got [32]byte
	if _, err := io.ReadFull(accepted, got[:]); err != nil {
		return true, err
	}
	if got != nonce {
		return true, errors.New("bind self-connection identity mismatch")
	}
	after, err := os.Lstat(name)
	if err != nil {
		return true, err
	}
	if !os.SameFile(before, after) {
		return true, errors.New("bind entry replaced")
	}
	_ = ln.SetDeadline(time.Time{})
	listener, err := ln.File()
	if err != nil {
		return true, err
	}
	defer listener.Close()
	return true, send(ipc, acknowledgement, listener, 1, after)
}

func send(ipc *net.UnixConn, ack <-chan error, file *os.File, kind byte, st os.FileInfo) error {
	var frame [17]byte
	frame[0] = kind
	if st != nil {
		sys := st.Sys().(*syscall.Stat_t)
		device := int64(sys.Dev)
		binary.LittleEndian.PutUint64(frame[1:9], uint64(device))
		binary.LittleEndian.PutUint64(frame[9:17], sys.Ino)
	}
	var rights []byte
	if file != nil {
		rights = syscall.UnixRights(int(file.Fd()))
	}
	return sendFrame(ipc, ack, frame, rights)
}

func sendFrame(ipc *net.UnixConn, ack <-chan error, frame [17]byte, rights []byte) error {
	if _, _, err := ipc.WriteMsgUnix(frame[:], rights, nil); err != nil {
		return err
	}
	return <-ack
}

func sendDialError(ipc *net.UnixConn, ack <-chan error, err error) error {
	var errno syscall.Errno
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		errno = syscall.ECONNREFUSED
	case errors.Is(err, os.ErrNotExist):
		errno = syscall.ENOENT
	default:
		return err
	}
	var frame [17]byte
	frame[0] = 3
	binary.LittleEndian.PutUint64(frame[1:9], uint64(errno))
	return sendFrame(ipc, ack, frame, nil)
}

func dialChild(ctx context.Context, ipc *net.UnixConn, ack <-chan error, name string) error {
	before, err := os.Lstat(name)
	if err != nil {
		return sendDialError(ipc, ack, err)
	}
	if before.Mode()&os.ModeSocket == 0 {
		return errors.New("dial entry is not a socket")
	}
	var dial net.Dialer
	conn, err := dial.DialContext(ctx, "unix", name)
	if err != nil {
		return sendDialError(ipc, ack, err)
	}
	defer conn.Close()
	after, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return errors.New("dial entry replaced")
	}
	own, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("dial result is not Unix")
	}
	file, err := own.File()
	if err != nil {
		return err
	}
	defer file.Close()
	return send(ipc, ack, file, 2, after)
}
