package installer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func world(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	manifest := filepath.Join(root, manifestName)
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"version":"1.2.3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(source, []byte(strings.Repeat("verified-image", 20000)), 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{Root: root, Want: "1.2.3", Compiled: "1.2.3", Source: source, Digest: fmt.Sprintf("%x", sha256.Sum256(read(t, source)))}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPublisherFirstInstallAndAtomicCommit(t *testing.T) {
	c := world(t)
	if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(c.Root, "bin/tmag")
	if err := os.WriteFile(target, []byte("previous-complete-image"), 0o755); err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	calls := 0
	c.Live = func(context.Context) error {
		calls++
		if calls == 2 {
			close(ready)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- Publish(context.Background(), c) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("ended before barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no commit barrier")
	}
	for range 20 {
		if string(read(t, target)) != "previous-complete-image" {
			t.Fatal("partial target visible before commit")
		}
	}
	n, err := openNamespace(context.Background(), c.Root)
	if n != nil {
		n.close()
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent publisher = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(read(t, target)) != string(read(t, c.Source)) {
		t.Fatal("published image differs")
	}
	st, err := os.Stat(filepath.Join(c.Root, "bin", gateName))
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("gate = %v %v", st, err)
	}
}

func TestPublisherVersionAndCancellationPreserveTarget(t *testing.T) {
	for _, kind := range []string{"compiled", "checkout", "checkout-before-commit", "digest", "canceled", "lost-controller"} {
		t.Run(kind, func(t *testing.T) {
			c := world(t)
			if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(c.Root, "bin/tmag")
			if err := os.WriteFile(target, []byte("previous"), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "compiled":
				c.Compiled = "1.2.2"
			case "checkout":
				if err := os.WriteFile(filepath.Join(c.Root, manifestName), []byte(`{"version":"1.2.4"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "digest":
				c.Digest = strings.Repeat("0", 64)
			case "checkout-before-commit":
				calls := 0
				c.Live = func(context.Context) error {
					calls++
					if calls == 2 {
						return os.WriteFile(filepath.Join(c.Root, manifestName), []byte(`{"version":"1.2.4"}`), 0o644)
					}
					return nil
				}
			case "lost-controller":
				calls := 0
				c.Live = func(context.Context) error {
					calls++
					if calls == 2 {
						return ErrOwnership
					}
					return nil
				}
			}
			if err := Publish(ctx, c); err == nil {
				t.Fatal("accepted invalid publication")
			}
			if string(read(t, target)) != "previous" {
				t.Fatal("prior target changed")
			}
		})
	}
}

func TestPublisherCheckoutAliasesUseOneGate(t *testing.T) {
	c := world(t)
	if err := Publish(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(c.Root, "bin", gateName))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(c.Root, alias); err != nil {
		t.Fatal(err)
	}
	c.Root = alias
	if err := Publish(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(c.Root, "bin", gateName))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("alias used a different gate: %v", err)
	}
}

func TestPublisherPreservesUnknownObjectsAndLegacyLocks(t *testing.T) {
	for _, kind := range []string{"directory", "regular", "symlink", "workspace-entry", "workspace-symlink", "target-symlink"} {
		t.Run(kind, func(t *testing.T) {
			c := world(t)
			if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			gate := filepath.Join(c.Root, "bin", gateName)
			switch kind {
			case "directory":
				if err := os.Mkdir(gate, 0o700); err != nil {
					t.Fatal(err)
				}
			case "regular":
				if err := os.WriteFile(gate, []byte("unknown"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(sentinel, gate); err != nil {
					t.Fatal(err)
				}
			case "target-symlink":
				if err := os.Symlink(sentinel, filepath.Join(c.Root, "bin/tmag")); err != nil {
					t.Fatal(err)
				}
			default:
				n, err := openNamespace(context.Background(), c.Root)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "workspace-entry" {
					err = n.work.WriteFile("unknown", []byte("preserve"), 0o600)
				} else {
					err = n.work.Symlink(sentinel, imageName)
				}
				n.close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := Publish(context.Background(), c); !errors.Is(err, ErrOwnership) {
				t.Fatalf("unknown object = %v", err)
			}
			if string(read(t, sentinel)) != "preserve" {
				t.Fatal("sentinel changed")
			}
			if kind == "regular" && string(read(t, gate)) != "unknown" {
				t.Fatal("unknown gate changed")
			}
		})
	}
}

func TestPublisherInitializedGateCreationRace(t *testing.T) {
	c := world(t)
	if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}, 2), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := os.OpenRoot(c.Root)
			if err != nil {
				results <- err
				return
			}
			defer p.Close()
			b, err := p.OpenRoot("bin")
			if err != nil {
				results <- err
				return
			}
			defer b.Close()
			pi, err := p.Stat(".")
			if err != nil {
				results <- err
				return
			}
			bi, err := b.Stat(".")
			if err != nil {
				results <- err
				return
			}
			n := &namespace{path: c.Root, plugin: p, bin: b, pluginInfo: pi, binInfo: bi, beforeLink: func() { ready <- struct{}{}; <-release }}
			results <- n.createGate(context.Background())
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case err := <-results:
			t.Fatalf("before link barrier: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("gate creation barrier")
		}
	}
	if _, err := os.Lstat(filepath.Join(c.Root, "bin", gateName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gate visible before complete creation: %v", err)
	}
	close(release)
	wg.Wait()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	n, err := openNamespace(context.Background(), c.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer n.close()
	var b binding
	if err := json.Unmarshal(read(t, filepath.Join(c.Root, "bin", gateName)), &b); err != nil || b.Protocol != "tmag/install/v1" {
		t.Fatalf("uninitialized gate: %+v %v", b, err)
	}
	items, err := os.ReadDir(filepath.Join(c.Root, "bin"))
	if err != nil || len(items) != 2 {
		t.Fatalf("losing creator left objects: %v %v", items, err)
	}
}

func TestPublisherNamespaceReplacementPreservesReplacement(t *testing.T) {
	c := world(t)
	calls := 0
	c.Live = func(context.Context) error {
		calls++
		if calls != 2 {
			return nil
		}
		if err := os.Rename(filepath.Join(c.Root, "bin"), filepath.Join(c.Root, "old-bin")); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(c.Root, "bin/tmag"), []byte("replacement"), 0o755)
	}
	// A final live callback represents the accepted-action check. Revalidation
	// must also happen after that callback before touching the pinned namespace.
	if err := Publish(context.Background(), c); !errors.Is(err, ErrOwnership) {
		t.Fatalf("replaced parent = %v", err)
	}
	if string(read(t, filepath.Join(c.Root, "bin/tmag"))) != "replacement" {
		t.Fatal("replacement changed")
	}
}

func TestInstallerCrashChild(t *testing.T) {
	if os.Getenv("TMAG_INSTALLER_CRASH_CHILD") != "1" {
		return
	}
	n, err := openNamespace(context.Background(), os.Getenv("TMAG_INSTALLER_ROOT"))
	if err != nil {
		os.Exit(2)
	}
	if err = n.work.WriteFile(imageName, []byte("partial"), 0o600); err != nil {
		os.Exit(3)
	}
	_, _ = os.Stdout.WriteString("ready\n")
	<-time.After(time.Minute)
}

func TestPublisherKernelLockCrashRecovery(t *testing.T) {
	c := world(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstallerCrashChild$")
	cmd.Env = append(os.Environ(), "TMAG_INSTALLER_CRASH_CHILD=1", "TMAG_INSTALLER_ROOT="+c.Root)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() { ready <- bufio.NewScanner(out).Scan() }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crash child not ready")
	}
	gate := filepath.Join(c.Root, "bin", gateName)
	before, err := os.Stat(gate)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := Publish(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(gate)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("gate replaced during recovery: %v", err)
	}
	if string(read(t, filepath.Join(c.Root, "bin/tmag"))) != string(read(t, c.Source)) {
		t.Fatal("crash recovery image differs")
	}
}

func TestPublisherOldInstallerCannotRemoveRegularGate(t *testing.T) {
	c := world(t)
	if err := Publish(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(c.Root, "bin", gateName)
	before, err := os.Stat(gate)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(gate, old, old); err != nil {
		t.Fatal(err)
	}
	// The base installer's actual age eviction and lock/trap primitives.
	cmd := exec.Command("bash", "-c", `find "$1" -maxdepth 0 -mmin +10 -exec rmdir {} \; 2>/dev/null; if mkdir "$1" 2>/dev/null; then exit 9; fi; rmdir "$1" 2>/dev/null; exit 0`, "legacy", gate)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("legacy commands: %v %s", err, out)
	}
	after, err := os.Stat(gate)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("legacy changed gate: %v", err)
	}
}

func TestPublisherRevalidatesInitializedGateSource(t *testing.T) {
	for _, kind := range []string{"workspace", "workspace-symlink", "gate", "header", "bin"} {
		t.Run(kind, func(t *testing.T) {
			c := world(t)
			if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			p, err := os.OpenRoot(c.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			b, err := p.OpenRoot("bin")
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			pi, err := p.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			bi, err := b.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			n := &namespace{path: c.Root, plugin: p, bin: b, pluginInfo: pi, binInfo: bi}
			var sentinel string
			var expected []byte
			n.beforeLink = func() {
				entries, err := os.ReadDir(filepath.Join(c.Root, "bin"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("initial workspace: %v %v", entries, err)
				}
				work := filepath.Join(c.Root, "bin", entries[0].Name())
				gate := filepath.Join(work, "gate")
				expected = []byte("preserve-foreign-source")
				switch kind {
				case "bin":
					if err := os.Rename(filepath.Join(c.Root, "bin"), filepath.Join(c.Root, "old-bin")); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(c.Root, "bin"), 0o755); err != nil {
						t.Fatal(err)
					}
					sentinel = filepath.Join(c.Root, "bin", "foreign")
				case "workspace":
					if err := os.Rename(work, work+"-original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(work, 0o700); err != nil {
						t.Fatal(err)
					}
					sentinel = gate
				case "workspace-symlink":
					if err := os.Rename(work, work+"-original"); err != nil {
						t.Fatal(err)
					}
					foreign := t.TempDir()
					if err := os.Symlink(foreign, work); err != nil {
						t.Fatal(err)
					}
					sentinel = filepath.Join(foreign, "gate")
				case "gate":
					if err := os.Rename(gate, gate+"-original"); err != nil {
						t.Fatal(err)
					}
					sentinel = gate
				case "header":
					expected = read(t, gate)
					expected[0] = '!'
					sentinel = gate
				}
				if err := os.WriteFile(sentinel, expected, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := n.createGate(context.Background()); !errors.Is(err, ErrOwnership) {
				t.Fatalf("replaced source %s = %v", kind, err)
			}
			if string(read(t, sentinel)) != string(expected) {
				t.Fatal("foreign source was changed")
			}
			if _, err := b.Lstat(gateName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published into original namespace: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(c.Root, "bin", gateName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published into replacement namespace: %v", err)
			}
		})
	}
}
