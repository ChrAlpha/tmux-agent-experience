package codexsvc

import (
	"net"
	"syscall"
)

// peerPID is the process at the other end of a Unix socket (SO_PEERCRED).
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		var cred *syscall.Ucred
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if serr == nil {
			pid = int(cred.Pid)
		}
	}); err != nil {
		return 0, err
	}
	return pid, serr
}
