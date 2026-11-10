package answer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// setupBudget bounds listening, separately from Wait's lifetime.
const setupBudget = time.Second

// exchangeBudget bounds an exchange that is not the operation itself: a
// probe, or telling hooks their prompts closed.
const exchangeBudget = time.Second

// staleAfter is how old a socket nothing serves must be before Listen
// removes it: younger, it may be one whose hook is still starting to listen.
const staleAfter = time.Minute

var requestName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// socketBase holds socketDir; tests point it elsewhere.
var socketBase = "/tmp"

// socketDir is where answer hooks listen: a directory of this user's own
// under /tmp, as tmux keeps its sockets, so that a socket's path stays within
// the platform's limit however long the state directory's is. A directory
// another user made, or one others may enter, is refused: a hook then does
// not wait, and the prompt is answered in the agent.
func socketDir() (string, error) {
	dir := filepath.Join(socketBase, fmt.Sprintf("tmag-%d", os.Geteuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || sys.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a directory of this user's alone", dir)
	}
	return dir, nil
}

// socketPath is where the hook of request id of state directory stateDir
// listens: the request ids of different state directories never meet.
func socketPath(stateDir, id string) (string, error) {
	if !requestName.MatchString(id) {
		return "", ErrInvalid
	}
	dir, err := socketDir()
	if err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir) + "\x00" + id))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".sock"), nil
}

type listener struct {
	*net.UnixListener
	id string
}

// Listen listens for the request id of state directory root.
func Listen(root, id string) (net.Listener, error) {
	return ListenContext(context.Background(), root, id)
}

// ListenContext listens for the request id of state directory root, within
// ctx's budget and setupBudget for setting up. It first removes the sockets
// of hooks that died without closing theirs. A socket left at its own path
// that nothing serves is replaced; one something serves is not.
func ListenContext(ctx context.Context, root, id string) (net.Listener, error) {
	ctx, cancel := context.WithTimeout(ctx, setupBudget)
	defer cancel()
	path, err := socketPath(root, id)
	if err != nil {
		return nil, err
	}
	sweep(ctx, filepath.Dir(path))
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if errors.Is(err, syscall.EADDRINUSE) {
		st, statErr := os.Lstat(path)
		if statErr != nil || probe(ctx, path, id) != Dead {
			return nil, fmt.Errorf("another answer hook waits on request %s", id)
		}
		removeIfSame(path, st)
		ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		ln.Close()
		return nil, err
	}
	return &listener{UnixListener: ln, id: id}, nil
}

// sweep removes the sockets of hooks that died without closing them: older
// than staleAfter, and refusing connections. The directory holds only the
// sockets of hooks waiting now, and those of hooks that died since the last
// sweep. A socket's path belongs to one hook, named by its request's random
// id (session.NewRequestID), so no other hook binds it once its own is gone;
// and only the very socket found refusing is removed, never one bound since.
func sweep(ctx context.Context, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode()&os.ModeSocket == 0 || time.Since(info.ModTime()) < staleAfter {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if refused(ctx, path) {
			removeIfSame(path, info)
		}
	}
}

// removeIfSame removes path if it is still the file st describes.
func removeIfSame(path string, st os.FileInfo) {
	if now, err := os.Lstat(path); err == nil && os.SameFile(st, now) {
		_ = os.Remove(path)
	}
}

// refused reports whether connecting to path is refused: no process
// listens there any longer.
func refused(ctx context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		c.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

func dial(ctx context.Context, root, id string) (net.Conn, error) {
	path, err := socketPath(root, id)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

// Presence is what a probe found of a request's hook. Unknown includes a
// timeout, an unexpected reply and a refused socket directory; only Dead, a
// socket missing or refusing connections, proves no hook waits.
type Presence uint8

const (
	Unknown Presence = iota
	Alive
	Dead
)

// Probe tells whether a hook waits on request id of state directory root.
func Probe(ctx context.Context, root, id string) Presence {
	path, err := socketPath(root, id)
	if err != nil {
		return Unknown
	}
	return probe(ctx, path, id)
}

// Waiting reports whether a hook waits on request id, within a bounded probe.
func Waiting(root, id string) bool { return Probe(context.Background(), root, id) == Alive }

func probe(ctx context.Context, path, id string) Presence {
	ctx, cancel := context.WithTimeout(ctx, exchangeBudget)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		if ctx.Err() == nil && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)) {
			return Dead
		}
		return Unknown
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if err := json.NewEncoder(c).Encode(message{Op: "probe", Request: id}); err != nil {
		return Unknown
	}
	var r struct {
		OK      bool   `json:"ok"`
		Request string `json:"request"`
	}
	decoder := json.NewDecoder(io.LimitReader(c, 512))
	if err := decoder.Decode(&r); err != nil || !r.OK || r.Request != id || ctx.Err() != nil {
		return Unknown
	}
	return Alive
}
