package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

func writeFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func releaseArchive(t *testing.T, data []byte, kind byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	h := &tar.Header{Name: "tmag", Mode: 0o755, Size: int64(len(data)), Typeflag: kind}
	if kind == tar.TypeSymlink {
		h.Linkname = "outside"
		h.Size = 0
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if kind != tar.TypeSymlink {
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type scriptWorld struct{ root, script, tools, scratch string }

func newScriptWorld(t *testing.T) scriptWorld {
	t.Helper()
	root := t.TempDir()
	w := scriptWorld{root: root, script: filepath.Join(root, "scripts/install.sh"), tools: filepath.Join(root, "tools"), scratch: filepath.Join(root, "scratch")}
	writeFixture(t, w.script, read(t, "../../scripts/install.sh"), 0o755)
	writeFixture(t, filepath.Join(root, manifestName), []byte(fmt.Sprintf(`{"version":%q}`, version.Version)), 0o644)
	if err := os.MkdirAll(w.scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(w.tools, "tmux"), []byte(`#!/usr/bin/env bash
case "$*" in
  "show-option -gqv @tmag-install-timeout")
    if [ "${TMAG_TEST_POLICY_STALL:-}" = 1 ]; then sleep 30; fi
    printf '%s\n' "${TMAG_TEST_BUDGET:-8}" ;;
  "show-option -gqv @tmag-release-url") printf '%s\n' "$TMAG_TEST_RELEASE" ;;
  display-message*)
    if [ -n "${TMAG_TEST_NOTICE:-}" ]; then printf done >"$TMAG_TEST_NOTICE"; fi
    if [ "${TMAG_TEST_NOTIFY_STALL:-}" = 1 ]; then sleep 30; fi ;;
  *) exit 2 ;;
esac
`), 0o755)
	return w
}

func (w scriptWorld) run(t *testing.T, release string, extra ...string) (time.Duration, string, error) {
	return w.runVersion(t, version.Version, release, extra...)
}

func (w scriptWorld) runVersion(t *testing.T, want, release string, extra ...string) (time.Duration, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", w.script, want)
	cmd.Env = append(os.Environ(), "PATH="+w.tools+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+w.scratch, "TMAG_TEST_RELEASE="+release)
	cmd.Env = append(cmd.Env, extra...)
	cmd.WaitDelay = 200 * time.Millisecond
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("installer escaped fixture deadline: %v output=%q", ctx.Err(), out)
	}
	return time.Since(start), string(out), err
}

func TestInstallerScript(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("supported Unix platforms")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required for local fixture")
	}
	// This is a synthetic local release of the actual CLI, including the private
	// publisher entry point. No external release/network or deployed binary.
	binary := filepath.Join(t.TempDir(), "tmag")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/tmag")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v %s", err, out)
	}
	image := read(t, binary)
	asset := fmt.Sprintf("tmag_%s_%s_%s.tar.gz", version.Version, runtime.GOOS, runtime.GOARCH)
	archive := releaseArchive(t, image, tar.TypeReg)
	t.Run("controller-cancellation", func(t *testing.T) {
		w := newScriptWorld(t)
		requested, release := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "checksums.txt") {
				_, _ = fmt.Fprintf(rw, "%x  %s\n", sha256.Sum256(archive), asset)
				return
			}
			close(requested)
			select {
			case <-release:
				_, _ = rw.Write(archive)
			case <-r.Context().Done():
			}
		}))
		defer server.Close()
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		target := filepath.Join(w.root, "bin/tmag")
		writeFixture(t, target, []byte("#!/bin/sh\necho old\n"), 0o755)
		notice := filepath.Join(w.root, "notice")
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", w.script, version.Version)
		cmd.Env = append(os.Environ(), "PATH="+w.tools+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+w.scratch, "TMAG_TEST_RELEASE="+server.URL, "TMAG_TEST_BUDGET=8", "TMAG_TEST_NOTICE="+notice)
		// Real files avoid a capture pipe keeping Wait blocked on surviving
		// descendants after the controller itself has been terminated.
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-requested:
		case <-ctx.Done():
			t.Fatal("fetch did not reach barrier")
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("canceled controller returned success")
		}
		close(release)
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(notice); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("bounded worker did not report refusal")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if string(read(t, target)) != "#!/bin/sh\necho old\n" {
			t.Fatal("lost controller authorized late publication")
		}
	})
	for _, kind := range []string{"first", "checksum", "wrong-version", "symlink", "trickle", "stall", "checksum-stall", "policy-stall", "notify-stall", "off", "hung-version"} {
		t.Run(kind, func(t *testing.T) {
			w := newScriptWorld(t)
			var requests atomic.Int32
			payload := archive
			if kind == "wrong-version" {
				payload = releaseArchive(t, []byte("#!/bin/sh\necho 'tmag 999.0.0 (tmag/v1)'\n"), tar.TypeReg)
			}
			if kind == "symlink" {
				payload = releaseArchive(t, nil, tar.TypeSymlink)
			}
			sum := fmt.Sprintf("%x", sha256.Sum256(payload))
			if kind == "checksum" {
				sum = strings.Repeat("0", 64)
			}
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				checksums := strings.HasSuffix(r.URL.Path, "checksums.txt")
				if kind == "stall" || (kind == "checksum-stall" && checksums) {
					<-r.Context().Done()
					return
				}
				if kind == "trickle" {
					flusher, ok := rw.(http.Flusher)
					if !ok {
						return
					}
					ticker := time.NewTicker(100 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-r.Context().Done():
							return
						case <-ticker.C:
							if _, err := io.WriteString(rw, "x"); err != nil {
								return
							}
							flusher.Flush()
						}
					}
				}
				if checksums {
					_, _ = fmt.Fprintf(rw, "%s  %s\n", sum, asset)
				} else {
					_, _ = rw.Write(payload)
				}
			}))
			defer server.Close()
			target := filepath.Join(w.root, "bin/tmag")
			previous := []byte("#!/bin/sh\necho 'tmag 0.0.1 (tmag/v1)'\n")
			if kind != "first" && kind != "notify-stall" {
				if kind == "hung-version" {
					previous = []byte("#!/bin/sh\nsleep 30\n")
				}
				writeFixture(t, target, previous, 0o755)
			}
			extra := []string{"TMAG_TEST_BUDGET=8"}
			switch kind {
			case "first":
				// No tmag, compiler, Python, flock or timeout on the runtime PATH.
				for _, name := range []string{"bash", "sleep", "head", "uname", "mkdir", "mktemp", "rm", "rmdir", "awk", "sha256sum", "shasum", "tar", "gzip", "chmod", "ps", "curl"} {
					path, err := exec.LookPath(name)
					if err != nil {
						continue
					}
					if err := os.Symlink(path, filepath.Join(w.tools, name)); err != nil {
						t.Fatal(err)
					}
				}
				extra = append(extra, "PATH="+w.tools)
				extra = append(extra, "TMPDIR="+filepath.Join(w.root, "unavailable-tmpdir"))
			case "trickle", "stall", "checksum-stall":
				extra = []string{"TMAG_TEST_BUDGET=3"}
			case "off":
				extra = []string{"TMAG_TEST_BUDGET=off"}
			case "policy-stall":
				extra = append(extra, "TMAG_TEST_POLICY_STALL=1")
			case "notify-stall":
				extra = append(extra, "TMAG_TEST_NOTIFY_STALL=1")
			}
			elapsed, out, err := w.run(t, server.URL, extra...)
			good := kind == "first" || kind == "off"
			if good && err != nil {
				t.Fatalf("install failed: %v elapsed=%v output=%q", err, elapsed, out)
			}
			if !good && err == nil {
				t.Fatalf("accepted %s", kind)
			}
			if kind == "trickle" || kind == "stall" || kind == "checksum-stall" {
				if elapsed > 5*time.Second {
					t.Fatalf("whole attempt exceeded budget: %v", elapsed)
				}
			}
			if kind == "policy-stall" && elapsed > 3*time.Second {
				t.Fatalf("policy lookup escaped bound: %v", elapsed)
			}
			switch {
			case kind == "off":
				if requests.Load() != 0 {
					t.Fatal("off started a fetch")
				}
				if string(read(t, target)) != string(previous) {
					t.Fatal("off changed binary")
				}
			case good || kind == "notify-stall":
				if !bytes.Equal(read(t, target), image) {
					t.Fatal("published image differs from verified release")
				}
			default:
				if string(read(t, target)) != string(previous) {
					t.Fatal("failed prepublication install changed prior binary")
				}
			}
			if kind == "hung-version" && elapsed > 4*time.Second {
				t.Fatalf("version phase escaped its cohort bound: %v", elapsed)
			}
		})
	}
}

func TestInstallerScriptWgetTrickle(t *testing.T) {
	wget, err := exec.LookPath("wget")
	if err != nil {
		t.Skip("wget not installed")
	}
	w := newScriptWorld(t)
	// A private minimal PATH exercises the real wget branch without curl. Only
	// existing system tools are linked; no additional runtime package is needed.
	for _, name := range []string{"bash", "sleep", "head", "uname", "mkdir", "mktemp", "rm", "rmdir", "awk", "tar", "gzip", "chmod", "ps", "mv", "cat", "wget"} {
		path, e := exec.LookPath(name)
		if e != nil {
			t.Fatalf("wget fixture prerequisite %s: %v", name, e)
		}
		if name == "wget" {
			path = wget
		}
		if err := os.Symlink(path, filepath.Join(w.tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	checksumTool := false
	for _, name := range []string{"sha256sum", "shasum"} {
		path, e := exec.LookPath(name)
		if e != nil {
			continue
		}
		if e := os.Symlink(path, filepath.Join(w.tools, name)); e != nil {
			t.Fatal(e)
		}
		checksumTool = true
	}
	if !checksumTool {
		t.Fatal("wget fixture needs sha256sum or shasum")
	}
	type observedRequest struct{ path, agent string }
	observed := make(chan observedRequest, 1)
	stopped := make(chan struct{}, 1)
	var requests, chunks atomic.Int32
	streamContext, stopStream := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case observed <- observedRequest{r.URL.Path, r.UserAgent()}:
		default:
		}
		defer func() {
			select {
			case stopped <- struct{}{}:
			default:
			}
		}()
		flusher, ok := rw.(http.Flusher)
		if !ok {
			return
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-streamContext.Done():
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := io.WriteString(rw, "x"); err != nil {
					return
				}
				flusher.Flush()
				chunks.Add(1)
			}
		}
	}))
	defer server.Close()
	// Release a failed fixture's handler before Close waits for it. This
	// cancellation runs only during teardown, after the termination assertions.
	defer stopStream()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", w.script, version.Version)
	cmd.Env = append(os.Environ(), "PATH="+w.tools, "TMPDIR="+w.scratch, "TMAG_TEST_RELEASE="+server.URL, "TMAG_TEST_BUDGET=3")
	cmd.WaitDelay = 200 * time.Millisecond
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err == nil || ctx.Err() != nil {
		t.Fatalf("wget containment: err=%v context=%v output=%q", err, ctx.Err(), out)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("wget trickle exceeded total bound: %v", elapsed)
	}
	asset := fmt.Sprintf("/v%s/tmag_%s_%s_%s.tar.gz", version.Version, version.Version, runtime.GOOS, runtime.GOARCH)
	select {
	case request := <-observed:
		if request.path != asset || !strings.HasPrefix(request.agent, "Wget/") {
			t.Fatalf("request did not reach real wget archive fetch: %+v", request)
		}
	default:
		t.Fatal("installer failed before wget requested the fixture")
	}
	if requests.Load() != 1 || chunks.Load() < 8 || elapsed < 1500*time.Millisecond {
		t.Fatalf("no sustained wget transfer at cutoff: requests=%d chunks=%d elapsed=%v", requests.Load(), chunks.Load(), elapsed)
	}
	// Wget's own inactivity timeout is ten seconds; the fixture continually
	// sends data. Ending this live stream within the unchanged five-second
	// bound demonstrates the installer's whole-attempt cutoff, not setup error.
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("wget stream survived installer return")
	}
}
