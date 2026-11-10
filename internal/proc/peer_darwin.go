package proc

import (
	"net"
	"syscall"
)

// Darwin's sys/un.h: getsockopt level and option for the peer's pid.
const (
	solLocal     = 0
	localPeerPID = 0x002
)

// PeerPID returns the process at the other end of a Unix socket
// (LOCAL_PEERPID).
func PeerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		pid, serr = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	}); err != nil {
		return 0, err
	}
	return pid, serr
}
