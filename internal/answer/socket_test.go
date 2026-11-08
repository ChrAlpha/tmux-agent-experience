package answer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/socketbind"
)

func TestMain(m *testing.M) {
	if handled, err := socketbind.RunChild(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "--test-answer-owner" {
		ln, err := Listen(os.Args[2], os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ready")
		time.Sleep(time.Hour)
		// Keep the listener live across the parent's forced-exit boundary; if
		// this watchdog returns, close it explicitly before exiting.
		_ = ln.Close()
		os.Exit(1)
	}
	// Helpers are intentionally short-lived even in race-instrumented test binaries.
	if err := os.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func answerDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "ans")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func runListener(t *testing.T, root, id string, a Asked) (net.Listener, <-chan bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := ListenContext(ctx, root, id)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- Wait(ctx, ln, a, func([]byte) error { return errors.New("probe must not emit") }) }()
	t.Cleanup(func() { cancel(); ln.Close() })
	return ln, done
}

// Probe's own budget can expire even for a known fixture. Classification tests
// retry only Unknown within a total budget; contrary definite evidence fails.
func requirePresence(t *testing.T, ctx context.Context, root, id string, want Presence) {
	t.Helper()
	for {
		got := Probe(ctx, root, id)
		if got == want {
			return
		}
		if got != Unknown || ctx.Err() != nil {
			t.Fatalf("request %s presence = %v, want %v: %v", id, got, want, ctx.Err())
		}
	}
}

// A short probe is not an operation's readiness handshake. Unknown must still
// permit the real identity-checked operation, while Dead is never accepted.
func requireRequest(t *testing.T, ctx context.Context, root, id string, want Asked) {
	t.Helper()
	p := Probe(ctx, root, id)
	if p == Dead {
		t.Fatalf("request %s unexpectedly proved dead", id)
	}
	got, err := Show(ctx, root, id)
	if err != nil || got.Tool != want.Tool || string(got.Input) != string(want.Input) {
		t.Fatalf("request %s Show after probe %v = %+v, want %+v: %v", id, p, got, want, err)
	}
}

func TestProbeIsIdentityOnlyAndClosedSocketIsNotWaiting(t *testing.T) {
	root := answerDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := "0123456789ab"
	asked := Asked{Tool: "Bash", Input: json.RawMessage(`{"command":"secret-tool-input"}`)}
	ln, _ := runListener(t, root, id, asked)
	path, err := location(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	if err := c.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(c).Encode(message{Op: "probe", Request: id}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(reply) != 2 || reply["ok"] != true || reply["request"] != id {
		t.Fatalf("probe leaked nonidentity fields: %+v", reply)
	}
	requirePresence(t, ctx, root, id, Alive)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("test did not retain an orphan: %v", err)
	}
	if Waiting(root, id) {
		t.Fatal("closed socket pathname was treated as live")
	}
	requirePresence(t, ctx, root, id, Dead)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "secret-tool-input") {
				return errors.New("tool input persisted")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProbeUnknownPreservesUnsupportedAndBlockedListeners(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			root := answerDir(t)
			id := "0123456789ab"
			if _, err := coord.Current(context.Background(), root); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "req"), 0700); err != nil {
				t.Fatal(err)
			}
			key := sha256.Sum256([]byte(id))
			meta, _ := json.Marshal(endpoint{Request: id})
			if err := os.WriteFile(filepath.Join(root, "req", fmt.Sprintf("meta-%02x", key[0])), meta, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "req", fmt.Sprintf("sock-%02x", key[0]))
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			if !blocked {
				go func() {
					c, err := ln.Accept()
					if err == nil {
						defer c.Close()
						var m message
						_ = json.NewDecoder(c).Decode(&m)
						_ = json.NewEncoder(c).Encode(map[string]any{"ok": false, "error": "unknown op probe"})
					}
				}()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if p := Probe(ctx, root, id); p != Unknown {
				t.Fatalf("uncertain listener = %v", p)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("probe destroyed an uncertain listener")
			}
		})
	}
}

func TestPoolAllocatesCollidingRequestsAndExcludesLiveReuse(t *testing.T) {
	root := answerDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := coord.RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	first := "request-a"
	key := sha256.Sum256([]byte(first))
	second := ""
	for i := range 4096 {
		name := fmt.Sprintf("request-b-%d", i)
		k := sha256.Sum256([]byte(name))
		if k[0] == key[0] {
			second = name
			break
		}
	}
	if second == "" {
		t.Fatal("fixture did not find a collision")
	}
	firstAsked, secondAsked := Asked{Tool: "Bash"}, Asked{Tool: "Read"}
	a, _ := runListener(t, root, first, firstAsked)
	b, _ := runListener(t, root, second, secondAsked)
	one, err := location(ctx, root, first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := location(ctx, root, second)
	if err != nil || one == two {
		t.Fatalf("colliding requests shared endpoint: %q %q %v", one, two, err)
	}
	if other, err := ListenContext(ctx, root, first); err == nil {
		other.Close()
		t.Fatal("allocated a duplicate while the original listener owns its slot")
	}
	requireRequest(t, ctx, root, first, firstAsked)
	requireRequest(t, ctx, root, second, secondAsked)
	a.Close()
	b.Close()
	// An ended request ID can safely reuse its own dead fixed position without
	// requiring a separate cleanup command.
	again, err := ListenContext(ctx, root, first)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestForcedExitSocketOrphanReclaimedUnderSlotExclusion(t *testing.T) {
	root := answerDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := coord.RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789ab"
	owner := exec.CommandContext(ctx, exe, "--test-answer-owner", root, id)
	pipe, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	if line, err := bufio.NewReader(pipe).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("socket owner not ready: %q %v", line, err)
	}
	path, err := location(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	// An alive owner excludes allocation even before its accept loop starts.
	key := sha256.Sum256([]byte(id))
	if again, err := ListenContext(ctx, root, id); err == nil {
		again.Close()
		t.Fatal("reused a live nonserving owner's slot")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("changed a live socket")
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	requirePresence(t, ctx, root, id, Dead)
	replacement, err := ListenContext(ctx, root, id)
	if err != nil {
		t.Fatalf("could not reuse dead owner's slot: %v", err)
	}
	replacement.Close()
	if after, err := location(ctx, root, id); err != nil || after != path {
		t.Fatalf("reused location = %q %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "coord", fmt.Sprintf("r-%02x", key[0]))); err != nil {
		t.Fatal("removed permanent request slot lock")
	}
}

func TestPoolPreservesUnknownObjectsAndBoundedPositions(t *testing.T) {
	root := answerDir(t)
	ctx := context.Background()
	if err := coord.RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	d, err := ownedfs.Open(ctx, root, "req", true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	for slot := range socketSlots {
		if err := os.WriteFile(filepath.Join(root, "req", fmt.Sprintf("sock-%02x", slot)), []byte("unknown"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if ln, err := ListenContext(ctx, root, "blocked-by-unknown"); err == nil {
		ln.Close()
		t.Fatal("adopted an unknown endpoint")
	}
	entries, err := os.ReadDir(filepath.Join(root, "req"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != socketSlots {
		t.Fatalf("created per-request artifacts while exhausted: %d", len(entries))
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, "req", e.Name()))
		if err != nil || string(b) != "unknown" {
			t.Fatalf("unknown changed: %q %v", b, err)
		}
	}
}

func TestListenRejectsAliasedRequestParent(t *testing.T) {
	root := answerDir(t)
	foreign := t.TempDir()
	if err := os.Symlink(foreign, filepath.Join(root, "req")); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(root, "0123456789ab"); err == nil {
		ln.Close()
		t.Fatal("published through an alias")
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatalf("foreign target changed: %d %v", len(entries), err)
	}
}

func TestFixedSocketDecisionRequiresMatchingRequestIdentity(t *testing.T) {
	root := answerDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := coord.RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	id := "request-identity"
	asked := Asked{Tool: "Bash"}
	ln, err := ListenContext(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	emitted := make(chan []byte, 1)
	done := make(chan bool, 1)
	go func() {
		defer close(done)
		done <- Wait(ctx, ln, asked, func(b []byte) error {
			emitted <- append([]byte(nil), b...)
			return nil
		})
	}()
	defer func() {
		cancel()
		ln.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("identity listener did not stop after close")
		}
	}()
	path, err := location(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(message{Op: "decide", Request: "another-request", Decision: &Decision{Behavior: Allow}}); err != nil {
		t.Fatal(err)
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil || r.OK || r.Error != "request identity changed" {
		t.Fatalf("mismatched decision succeeded: %+v %v", r, err)
	}
	select {
	case <-done:
		t.Fatal("mismatched decision ended the hook")
	case b := <-emitted:
		t.Fatalf("mismatched decision emitted %s", b)
	default:
	}
	// Unknown is uncertainty, not an ended request. Force that outcome without
	// depending on helper startup timing, then complete the matching request.
	probeCtx, cancelProbe := context.WithCancel(ctx)
	cancelProbe()
	if p := Probe(probeCtx, root, id); p != Unknown {
		t.Fatalf("canceled probe = %v, want Unknown", p)
	}
	requireRequest(t, ctx, root, id, asked)
	if err := Decide(ctx, root, id, Decision{Behavior: Allow}); err != nil {
		t.Fatalf("correct identity could not decide: %v", err)
	}
	select {
	case printed := <-done:
		if !printed {
			t.Fatal("correct identity ended without printing its decision")
		}
	case <-ctx.Done():
		t.Fatalf("correct identity did not finish the hook: %v", ctx.Err())
	}
	select {
	case b := <-emitted:
		if string(b) != `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}` {
			t.Fatalf("correct identity emitted %s", b)
		}
	default:
		t.Fatal("correct identity acknowledged without emitting a decision")
	}
}

func TestCorruptOrAliasedLiveIdentityIsUnknownAndCannotRedirect(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			root := answerDir(t)
			ctx := context.Background()
			id := "request-identity-owner"
			if err := coord.RetireLegacy(ctx, root); err != nil {
				t.Fatal(err)
			}
			_, done := runListener(t, root, id, Asked{Tool: "Bash"})
			key := sha256.Sum256([]byte(id))
			meta := filepath.Join(root, "req", fmt.Sprintf("meta-%02x", key[0]))
			foreign := filepath.Join(t.TempDir(), "metadata")
			if alias {
				if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(meta); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, meta); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(meta, []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
			if p := Probe(ctx, root, id); p != Unknown {
				t.Fatalf("uncertain live identity = %v", p)
			}
			if ln, err := ListenContext(ctx, root, id); err == nil {
				ln.Close()
				t.Fatal("allocated a second endpoint for uncertain live identity")
			}
			select {
			case <-done:
				t.Fatal("uncertain metadata disrupted original listener")
			default:
			}
			if alias {
				if b, err := os.ReadFile(foreign); err != nil || string(b) != "preserve" {
					t.Fatalf("aliased metadata changed: %q %v", b, err)
				}
			}
		})
	}
}

func TestFailedAllocationClosesEveryPinnedHandle(t *testing.T) {
	root := answerDir(t)
	ctx := context.Background()
	if err := coord.RetireLegacy(ctx, root); err != nil {
		t.Fatal(err)
	}
	d, err := ownedfs.Open(ctx, root, "req", true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	collision := filepath.Join(root, "req", "meta-00")
	if err := os.WriteFile(collision, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	fdDir := "/dev/fd"
	if _, err := os.Stat("/proc/self/fd"); err == nil {
		fdDir = "/proc/self/fd"
	}
	count := func() int {
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for range 24 {
		if ln, err := ListenContext(ctx, root, "corrupt-reserved-identity"); err == nil {
			ln.Close()
			t.Fatal("adopted a corrupt identity")
		}
	}
	after := count()
	if after > before+2 {
		t.Fatalf("failed initialization leaked pinned descriptors: before=%d after=%d", before, after)
	}
	if b, err := os.ReadFile(collision); err != nil || string(b) != "unknown" {
		t.Fatalf("unknown prefix collision changed: %q %v", b, err)
	}
}
