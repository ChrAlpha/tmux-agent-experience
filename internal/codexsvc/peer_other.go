//go:build !linux && !darwin

package codexsvc

import (
	"errors"
	"net"
)

// peerPID is not known on this platform; tmag supports Linux and macOS.
func peerPID(*net.UnixConn) (int, error) {
	return 0, errors.New("the peer of a Unix socket is not known on this platform")
}
