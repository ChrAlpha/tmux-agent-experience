package cli

import (
	"errors"
	"os"
	"syscall"
)

// hangup returns a channel closed once nobody reads f any more: the reader
// of a pipe closed it, or the peer of a socket shut it down both ways, as
// sshd does when the connection of a command run without a terminal drops.
// It returns nil when f is neither a pipe nor a socket, and stops watching
// when done is closed.
func hangup(f *os.File, done <-chan struct{}) <-chan struct{} {
	if !readByPeer(f) {
		return nil
	}
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil
	}
	fd := int(f.Fd())
	// No events are asked for: epoll always reports EPOLLERR, a pipe without
	// readers, and EPOLLHUP, a socket shut down both ways. A socket shut down
	// for reading alone is no hang-up: sshd runs a command without a terminal
	// on one socket for its input and output, and passes the end of the
	// client's input on as such a half-close.
	if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Fd: int32(fd)}); err != nil {
		_ = syscall.Close(ep)
		return nil
	}
	gone := make(chan struct{})
	go func() {
		defer syscall.Close(ep)
		events := make([]syscall.EpollEvent, 1)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := syscall.EpollWait(ep, events, int(hangupPoll.Milliseconds()))
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n > 0 && events[0].Events&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
				close(gone)
				return
			}
		}
	}()
	return gone
}
