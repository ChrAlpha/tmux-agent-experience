//go:build !linux && !darwin

package proc

import (
	"errors"
	"net"
)

// PeerPID is not known on this platform; tmag supports Linux and macOS.
func PeerPID(*net.UnixConn) (int, error) {
	return 0, errors.New("the peer of a Unix socket is not known on this platform")
}
