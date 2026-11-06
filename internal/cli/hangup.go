package cli

import (
	"errors"
	"os"
	"time"
)

// errReaderGone ends a watch whose output nobody reads any more.
var errReaderGone = errors.New("the reader of the output went away")

// hangupPoll is how often the wait for a hang-up looks whether it should
// stop; the hang-up itself is reported at once.
const hangupPoll = time.Second

// readByPeer reports whether f leads to a process that can go away: a pipe
// or a socket. A terminal gets SIGHUP instead, and a file has no reader.
func readByPeer(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0
}
