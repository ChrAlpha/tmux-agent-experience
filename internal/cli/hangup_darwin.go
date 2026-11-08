package cli

import (
	"errors"
	"os"
	"syscall"
)

// hangup returns a channel closed once nobody reads f any more: the reader
// of a pipe closed it, or the peer of a socket disconnected, as sshd does
// when the connection of a command run without a terminal drops. It returns
// nil when f is neither a pipe nor a socket, and stops watching when done is
// closed.
func hangup(f *os.File, done <-chan struct{}) <-chan struct{} {
	if !readByPeer(f) {
		return nil
	}
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil
	}
	syscall.CloseOnExec(kq)
	// The write filter sets EV_EOF once the reader of a pipe is gone or the
	// peer of a socket disconnected, but not when the peer only stopped
	// sending, as sshd passes on the end of the client's input. EV_CLEAR
	// reports writability only when it changes, so the wait does not spin
	// while the output can be written.
	var change syscall.Kevent_t
	if err := controlFile(f, func(fd uintptr) error {
		syscall.SetKevent(&change, int(fd), syscall.EVFILT_WRITE, syscall.EV_ADD|syscall.EV_CLEAR)
		_, err := syscall.Kevent(kq, []syscall.Kevent_t{change}, nil, nil)
		return err
	}); err != nil {
		_ = syscall.Close(kq)
		return nil
	}
	gone := make(chan struct{})
	go func() {
		defer syscall.Close(kq)
		events := make([]syscall.Kevent_t, 1)
		timeout := syscall.NsecToTimespec(hangupPoll.Nanoseconds())
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := syscall.Kevent(kq, nil, events, &timeout)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n > 0 && events[0].Flags&syscall.EV_EOF != 0 {
				close(gone)
				return
			}
		}
	}()
	return gone
}
