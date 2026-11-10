package answer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == "--test-answer-owner" {
		socketBase = os.Args[2]
		ln, err := Listen(os.Args[3], os.Args[4])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ready")
		Wait(context.Background(), ln, Asked{Tool: "Bash"}, func([]byte) error { return nil })
		_ = ln.Close()
		os.Exit(1)
	}
	base, err := os.MkdirTemp("", "tmag-answer")
	if err != nil {
		panic(err)
	}
	socketBase = base
	code := m.Run()
	os.RemoveAll(base)
	os.Exit(code)
}

// owner starts a process that listens for id of root, as an answer hook
// does, and returns it running.
func owner(t *testing.T, root, id string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "--test-answer-owner", socketBase, root, id)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("owner did not start: %q %v", line, err)
	}
	return cmd
}

// A hook that dies without closing its socket leaves it refusing
// connections: it is Dead, nothing is delivered to it, and the next hook of
// the request replaces it.
func TestDeadHookSocketIsDeadAndReplaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	cmd := owner(t, root, "r1")
	if got := Probe(ctx, root, "r1"); got != Alive {
		t.Fatalf("a listening hook is %v", got)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	path, _ := socketPath(root, "r1")
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("a killed hook left no socket: %v", err)
	}
	if got := Probe(ctx, root, "r1"); got != Dead {
		t.Fatalf("a killed hook is %v", got)
	}
	if err := Decide(ctx, root, "r1", Decision{Behavior: Allow}); !errors.Is(err, ErrGone) {
		t.Fatalf("deciding for a killed hook: %v", err)
	}
	ln, _ := runListener(t, root, Asked{Tool: "Bash"})
	defer ln.Close()
	if got := Probe(ctx, root, "r1"); got != Alive {
		t.Fatalf("the replacing hook is %v", got)
	}
}

// A request a live hook waits on is not taken over.
func TestLiveHookIsNotReplaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	owner(t, root, "r1")
	if ln, err := ListenContext(ctx, root, "r1"); err == nil {
		ln.Close()
		t.Fatal("a second hook took over a live one's request")
	}
}

// Listening removes the sockets hooks that died left behind, once they are
// old enough not to be a hook still setting up.
func TestListenSweepsOldDeadSockets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	dead := func(id string) string {
		cmd := owner(t, root, id)
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
		path, _ := socketPath(root, id)
		return path
	}
	old, young := dead("old"), dead("young")
	past := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	ln, _ := runListener(t, root, Asked{Tool: "Bash"})
	defer ln.Close()
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an old dead socket remains: %v", err)
	}
	if _, err := os.Lstat(young); err != nil {
		t.Fatalf("a young socket was removed: %v", err)
	}
	live, _ := socketPath(root, "r1")
	if err := os.Chtimes(live, past, past); err != nil {
		t.Fatal(err)
	}
	other, err := ListenContext(ctx, root, "r2")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if got := Probe(ctx, root, "r1"); got != Alive {
		t.Fatalf("a live hook was swept: %v", got)
	}
}

// A hook's socket path does not depend on how long the state directory's
// path is.
func TestLongStateDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := filepath.Join(t.TempDir(), strings.Repeat("d", 200), strings.Repeat("e", 200))
	ln, done := runListener(t, root, Asked{Tool: "Bash"})
	defer ln.Close()
	if got, err := Show(ctx, root, "r1"); err != nil || got.Tool != "Bash" {
		t.Fatalf("Show = %+v, %v", got, err)
	}
	Close(ctx, root, "r1")
	if emitted := <-done; emitted {
		t.Fatal("a closed request emitted a decision")
	}
}

// A hook answers only messages naming its own request, and requests of
// different state directories never meet.
func TestRequestIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root, other := t.TempDir(), t.TempDir()
	ln, done := runListener(t, root, Asked{Tool: "Bash"})
	defer ln.Close()
	if got := Probe(ctx, other, "r1"); got != Dead {
		t.Fatalf("another state directory's request r1 is %v", got)
	}
	path, _ := socketPath(root, "r1")
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(message{Op: "decide", Request: "r2", Decision: &Decision{Behavior: Allow}}); err != nil {
		t.Fatal(err)
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil || r.OK {
		t.Fatalf("a decision for another request was taken: %+v %v", r, err)
	}
	Close(ctx, root, "r1")
	if emitted := <-done; emitted {
		t.Fatal("a decision for another request was printed")
	}
}

// A socket directory others may enter, or another user's, is refused, and
// with it every request: hooks then leave the decision to the agent.
func TestSocketDirectoryMustBePrivate(t *testing.T) {
	saved := socketBase
	defer func() { socketBase = saved }()
	socketBase = t.TempDir()
	dir := filepath.Join(socketBase, fmt.Sprintf("tmag-%d", os.Geteuid()))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(t.TempDir(), "r1"); err == nil {
		ln.Close()
		t.Fatal("listened in a directory others may enter")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(t.TempDir(), "r1"); err == nil {
		ln.Close()
		t.Fatal("listened through a symlinked directory")
	}
}

// runListener starts a hook waiting on request r1 of root.
func runListener(t *testing.T, root string, a Asked) (net.Listener, <-chan bool) {
	t.Helper()
	ln, err := Listen(root, "r1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- Wait(context.Background(), ln, a, func([]byte) error { return nil }) }()
	return ln, done
}
