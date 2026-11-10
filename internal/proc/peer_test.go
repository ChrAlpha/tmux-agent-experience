package proc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The peer of a Unix socket is the process that connected it.
func TestPeerPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for name, c := range map[string]net.Conn{"client": conn, "server": server} {
		pid, err := PeerPID(c.(*net.UnixConn))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pid != os.Getpid() {
			t.Errorf("%s peer pid = %d, want %d", name, pid, os.Getpid())
		}
	}
}
