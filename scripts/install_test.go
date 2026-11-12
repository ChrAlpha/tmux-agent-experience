package scripts_test

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
)

const want = "0.3.0"

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
}

func archive(t *testing.T, version string, kind byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	image := []byte("#!/bin/sh\nprintf 'tmag " + version + " (tmag/v2)\\n'\n")
	h := &tar.Header{Name: "tmag", Mode: 0755, Size: int64(len(image)), Typeflag: kind}
	if kind == tar.TypeSymlink {
		h.Linkname = "outside"
		h.Size = 0
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if kind != tar.TypeSymlink {
		if _, err := tw.Write(image); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type world struct{ root, script, tools, release, asset string }

func fixture(t *testing.T, image []byte, corrupt bool) world {
	t.Helper()
	root := t.TempDir()
	w := world{root: root, script: filepath.Join(root, "scripts/install.sh"), tools: filepath.Join(root, "tools"), release: filepath.Join(root, "release"), asset: fmt.Sprintf("tmag_%s_%s_%s.tar.gz", want, runtime.GOOS, runtime.GOARCH)}
	source, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	write(t, w.script, source)
	write(t, filepath.Join(w.tools, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"$TMAG_TEST_RELEASE\"\n"))
	write(t, filepath.Join(w.release, "v"+want, w.asset), image)
	sum := fmt.Sprintf("%x", sha256.Sum256(image))
	if corrupt {
		sum = strings.Repeat("0", 64)
	}
	write(t, filepath.Join(w.release, "v"+want, "checksums.txt"), []byte(sum+"  "+w.asset+"\n"))
	return w
}

func (w world) command(ctx context.Context, extra ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", w.script, want)
	cmd.Env = append(os.Environ(), "PATH="+w.tools+string(os.PathListSeparator)+os.Getenv("PATH"), "TMAG_TEST_RELEASE=file://"+w.release, "TMPDIR="+filepath.Join(w.root, "unavailable-tmp"))
	cmd.Env = append(cmd.Env, extra...)
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd
}

func run(t *testing.T, w world, extra ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := w.command(ctx, extra...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("fixture exceeded deadline: %v %q", ctx.Err(), out)
	}
	return string(out), err
}

func TestInstallerReleasedArchive(t *testing.T) {
	for _, kind := range []string{"first", "integrity", "version", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			version, member := want, byte(tar.TypeReg)
			if kind == "version" {
				version = "9.9.9"
			}
			if kind == "symlink" {
				member = tar.TypeSymlink
			}
			w := fixture(t, archive(t, version, member), kind == "integrity")
			target := filepath.Join(w.root, "bin/tmag")
			previous := []byte("#!/bin/sh\necho old\n")
			if kind != "first" {
				write(t, target, previous)
			}
			out, err := run(t, w)
			if kind == "first" {
				if err != nil {
					t.Fatalf("first install: %v %q", err, out)
				}
				st, err := os.Stat(target)
				if err != nil || st.Mode().Perm() != 0755 {
					t.Fatalf("installed mode: %v %v", st, err)
				}
				// A second invocation uses the installed image without requiring network.
				if out, err := run(t, w, "TMAG_TEST_RELEASE=http://127.0.0.1:1/unreachable"); err != nil {
					t.Fatalf("no-op: %v %q", err, out)
				}
			} else {
				if err == nil {
					t.Fatalf("accepted %s", kind)
				}
				if b, err := os.ReadFile(target); err != nil || !bytes.Equal(b, previous) {
					t.Fatalf("prior binary changed: %q %v", b, err)
				}
			}
			noWork(t, w)
		})
	}
}

// noWork fails when a run left its work directory in bin.
func noWork(t *testing.T, w world) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(w.root, "bin/.install.*")); len(left) > 0 {
		t.Fatalf("work directories left: %v", left)
	}
}

// Runs at once need no lock: each downloads into its own directory and
// publishes the same verified release with one rename.
func TestConcurrentInstallersPublishTheVerifiedRelease(t *testing.T) {
	image := archive(t, want, tar.TypeReg)
	w := fixture(t, image, false)
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.tools, "curl"), []byte(`#!/bin/sh
if [ -n "${TMAG_TEST_ENTERED:-}" ]; then
 printf entered >"$TMAG_TEST_ENTERED"
 while [ ! -f "$TMAG_TEST_RELEASE_FETCH" ]; do sleep 0.02; done
fi
exec "$TMAG_TEST_CURL" "$@"
`))
	entered, release := filepath.Join(w.root, "entered"), filepath.Join(w.root, "release-fetch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := w.command(ctx, "TMAG_TEST_CURL="+realCurl, "TMAG_TEST_ENTERED="+entered, "TMAG_TEST_RELEASE_FETCH="+release)
	var output bytes.Buffer
	first.Stdout = &output
	first.Stderr = &output
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { write(t, release, nil); cancel(); _ = first.Wait() }()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("fetch barrier was not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, err := run(t, w, "TMAG_TEST_CURL="+realCurl); err != nil {
		t.Fatalf("second installer while the first downloads: %v %q", err, out)
	}
	write(t, release, nil)
	if err := first.Wait(); err != nil {
		t.Fatalf("first installer: %v %q", err, output.String())
	}
	if b, err := os.ReadFile(filepath.Join(w.root, "bin/tmag")); err != nil || !strings.Contains(string(b), "tmag "+want) {
		t.Fatalf("published binary: %q %v", b, err)
	}
	noWork(t, w)
}

// A run that died leaves its work directory, which a later run removes once
// it is old enough that no run in progress can own it.
func TestInstallerRemovesWhatDeadRunsLeft(t *testing.T) {
	w := fixture(t, archive(t, want, tar.TypeReg), false)
	dead, young := filepath.Join(w.root, "bin/.install.dead"), filepath.Join(w.root, "bin/.install.young")
	write(t, filepath.Join(dead, "archive"), []byte("partial"))
	write(t, filepath.Join(young, "archive"), []byte("partial"))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dead, old, old); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, w); err != nil {
		t.Fatalf("install: %v %q", err, out)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead run's directory survived: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("a run in progress lost its directory: %v", err)
	}
}

func TestInstallerCurlTotalTransferDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the real 25-second transfer deadline")
	}
	w := fixture(t, archive(t, want, tar.TypeReg), false)
	previous := []byte("#!/bin/sh\necho old\n")
	write(t, filepath.Join(w.root, "bin/tmag"), previous)
	var chunks atomic.Int32
	stopped := make(chan struct{})
	stream, stop := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		rw.WriteHeader(http.StatusOK)
		flusher, ok := rw.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stream.Done():
				return
			case <-tick.C:
				if _, err := io.WriteString(rw, "x"); err != nil {
					return
				}
				flusher.Flush()
				chunks.Add(1)
			}
		}
	}))
	defer server.Close()
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := time.Now()
	out, err := w.command(ctx, "TMAG_TEST_RELEASE="+server.URL).CombinedOutput()
	elapsed := time.Since(start)
	if err == nil || ctx.Err() != nil || chunks.Load() < 100 || elapsed < 20*time.Second || elapsed > 30*time.Second {
		t.Fatalf("curl transfer bound: %v context=%v chunks=%d elapsed=%v output=%q", err, ctx.Err(), chunks.Load(), elapsed, out)
	}
	if b, err := os.ReadFile(filepath.Join(w.root, "bin/tmag")); err != nil || !bytes.Equal(b, previous) {
		t.Fatalf("prior binary changed: %q %v", b, err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("curl transfer survived return")
	}
}

func TestInstallerReportsMissingCurl(t *testing.T) {
	w := fixture(t, archive(t, want, tar.TypeReg), false)
	out, err := run(t, w, "PATH="+w.tools)
	if err == nil || !strings.Contains(out, "curl is required") {
		t.Fatalf("missing downloader: %v %q", err, out)
	}
	noWork(t, w)
}

func TestInstallerPublishesCompleteVerifiedImage(t *testing.T) {
	// Hold the candidate's version check: all network/integrity work has finished,
	// but the previous executable must still be present until the single rename.
	root := t.TempDir()
	entered, release := filepath.Join(root, "entered"), filepath.Join(root, "publish")
	image := []byte(`#!/bin/sh
if [ -n "${TMAG_TEST_ENTERED:-}" ]; then
 printf ready >"$TMAG_TEST_ENTERED"
 while [ ! -f "$TMAG_TEST_PUBLISH" ]; do sleep 0.02; done
fi
printf 'tmag 0.3.0 (tmag/v2)\n'
`)
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "tmag", Mode: 0755, Size: int64(len(image)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	w := fixture(t, b.Bytes(), false)
	target := filepath.Join(w.root, "bin/tmag")
	previous := []byte("#!/bin/sh\necho old\n")
	write(t, target, previous)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := w.command(ctx, "TMAG_TEST_ENTERED="+entered, "TMAG_TEST_PUBLISH="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Always release our synthetic candidate, including a failed assertion.
	defer func() { write(t, release, nil); cancel(); _ = cmd.Wait() }()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("candidate did not reach publication barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, previous) {
		t.Fatalf("changed prior executable before commit: %q %v", got, err)
	}
	write(t, release, nil)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("publication failed: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, image) {
		t.Fatalf("publication is not the complete verified image: %q %v", got, err)
	}
}
