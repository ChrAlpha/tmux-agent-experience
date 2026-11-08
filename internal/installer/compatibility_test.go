package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This fixture reproduces v0.3.0's version and unknown-command response exactly.
// It models the read-only legacy CLI contract, not the new publisher implementation.
func legacyContractFixture(want, reply string, status int, publish string) []byte {
	return []byte(fmt.Sprintf(`#!/usr/bin/env bash
case "${1:-}" in
  version) printf 'tmag %%s (tmag/v1)\n' '%s' ;;
  __install-capabilities)
    cat <<'TMAG_LEGACY_CAPABILITY' >&2
%s
TMAG_LEGACY_CAPABILITY
    exit %d ;;
  __install-release) %s ;;
  *) exit 2 ;;
esac
`, want, reply, status, publish))
}

func TestInstallerPublishedReleaseCompatibility(t *testing.T) {
	response := read(t, "testdata/legacy-0.3.0-capability.txt")
	if got := fmt.Sprintf("%x", sha256.Sum256(response)); got != "7d84ac68ead04ecf0a075d9d9553c9d764f5853d3e488cfcbc51d1372a9a61b4" {
		t.Fatal("legacy CLI contract fixture changed")
	}
	for _, kind := range []string{"initial", "regular-gate", "directory-lock", "capability-corrupt", "capability-error", "unknown-version", "helper-failure", "helper-committed-error", "obsolete-before-commit", "malformed-manifest", "replaced-lock", "replaced-regular-gate", "legacy-crash-residue"} {
		t.Run(kind, func(t *testing.T) {
			w := newScriptWorld(t)
			want, reply, status := "0.3.0", string(response), 2
			publish := "exit 97"
			if kind == "unknown-version" {
				want = "0.3.1"
			}
			if kind == "capability-corrupt" {
				reply += "corrupt"
			}
			if kind == "capability-error" {
				reply = "capability unavailable"
				status = 1
			}
			if strings.HasPrefix(kind, "helper-") {
				reply = "tmag/install/v1"
				status = 0
				publish = `printf invoked >"$TMAG_TEST_HELPER_CALLED"; exit 1`
				if kind == "helper-committed-error" {
					publish = `cp "$0" "$2/bin/tmag"; printf invoked >"$TMAG_TEST_HELPER_CALLED"; exit 1`
				}
			}
			image := legacyContractFixture(want, reply, status, publish)
			archive := releaseArchive(t, image, tar.TypeReg)
			asset := fmt.Sprintf("tmag_%s_%s_%s.tar.gz", want, runtime.GOOS, runtime.GOARCH)
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "checksums.txt") {
					_, _ = fmt.Fprintf(rw, "%x  %s\n", sha256.Sum256(archive), asset)
				} else {
					_, _ = rw.Write(archive)
				}
			}))
			defer server.Close()
			manifest := filepath.Join(w.root, manifestName)
			var metadata map[string]any
			if err := json.Unmarshal(read(t, "../../plugins/codex/tmag/.codex-plugin/plugin.json"), &metadata); err != nil {
				t.Fatal(err)
			}
			metadata["version"] = want
			manifestData, err := json.MarshalIndent(metadata, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			writeFixture(t, manifest, manifestData, 0o644)
			if kind == "malformed-manifest" {
				writeFixture(t, manifest, []byte(`{"version":"0.3.0", broken}`), 0o644)
			}
			target, gate := filepath.Join(w.root, "bin/tmag"), filepath.Join(w.root, "bin", gateName)
			previous := []byte("#!/bin/sh\necho old\n")
			if kind != "initial" {
				writeFixture(t, target, previous, 0o755)
			}
			if kind == "regular-gate" {
				writeFixture(t, gate, []byte("preserve-permanent-gate"), 0o600)
			}
			if kind == "directory-lock" {
				writeFixture(t, filepath.Join(gate, "unknown"), []byte("preserve-directory-owner"), 0o600)
			}
			called := filepath.Join(w.root, "helper-called")
			extra := []string{"TMPDIR=" + filepath.Join(w.root, "unavailable-tmpdir"), "TMAG_TEST_HELPER_CALLED=" + called}
			if kind == "obsolete-before-commit" || kind == "replaced-lock" || kind == "replaced-regular-gate" || kind == "legacy-crash-residue" {
				realPS, err := exec.LookPath("ps")
				if err != nil {
					t.Fatal(err)
				}
				writeFixture(t, filepath.Join(w.tools, "ps"), []byte(`#!/usr/bin/env bash
case "$TMAG_TEST_MUTATION" in
 obsolete-before-commit) printf '{"version":"0.3.1"}' >"$TMAG_TEST_MANIFEST" ;;
 replaced-lock)
  mv "$TMAG_TEST_GATE" "$TMAG_TEST_GATE-original"
  mkdir "$TMAG_TEST_GATE"
  printf preserve-replacement >"$TMAG_TEST_GATE/foreign" ;;
 replaced-regular-gate)
  mv "$TMAG_TEST_GATE" "$TMAG_TEST_GATE-original"
  printf preserve-replacement >"$TMAG_TEST_GATE" ;;
 legacy-crash-residue) sleep 30 ;;
esac
exec "$TMAG_TEST_REAL_PS" "$@"
`), 0o755)
				extra = append(extra, "TMAG_TEST_MUTATION="+kind, "TMAG_TEST_MANIFEST="+manifest, "TMAG_TEST_GATE="+gate, "TMAG_TEST_REAL_PS="+realPS)
			}
			_, out, err := w.runVersion(t, want, server.URL, extra...)
			if kind == "initial" {
				if err != nil {
					t.Fatalf("released legacy initial install: %v %q", err, out)
				}
				if !bytes.Equal(read(t, target), image) {
					t.Fatal("legacy publication differs from verified archive")
				}
				info, e := os.Stat(target)
				if e != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("installed mode: %v %v", info, e)
				}
			} else {
				if err == nil {
					t.Fatalf("accepted %s", kind)
				}
				if kind == "helper-committed-error" {
					if !bytes.Equal(read(t, target), image) {
						t.Fatal("indeterminate helper commit was overwritten")
					}
				} else if !bytes.Equal(read(t, target), previous) {
					t.Fatal("prepublication failure changed previous binary")
				}
			}
			switch kind {
			case "regular-gate":
				if string(read(t, gate)) != "preserve-permanent-gate" {
					t.Fatal("downgraded kernel gate")
				}
			case "directory-lock":
				if string(read(t, filepath.Join(gate, "unknown"))) != "preserve-directory-owner" {
					t.Fatal("changed unknown lock owner")
				}
			case "replaced-lock":
				if string(read(t, filepath.Join(gate, "foreign"))) != "preserve-replacement" {
					t.Fatal("removed replacement owner")
				}
			case "replaced-regular-gate":
				if string(read(t, gate)) != "preserve-replacement" {
					t.Fatal("removed replacement kernel gate")
				}
			case "legacy-crash-residue":
				if _, e := os.Stat(filepath.Join(gate, "owner")); e != nil {
					t.Fatalf("legacy crash ownership missing: %v", e)
				}
				if err := os.Remove(filepath.Join(w.tools, "ps")); err != nil {
					t.Fatal(err)
				}
				if _, _, again := w.runVersion(t, want, server.URL); again == nil {
					t.Fatal("adopted ambiguous legacy crash lock")
				}
			default:
				if _, e := os.Lstat(gate); !os.IsNotExist(e) {
					t.Fatalf("unexpected legacy gate after %s: %v", kind, e)
				}
			}
			if strings.HasPrefix(kind, "helper-") && string(read(t, called)) != "invoked" {
				t.Fatal("capable helper was not selected")
			}
			if kind == "initial" {
				entries, e := os.ReadDir(filepath.Join(w.root, "bin"))
				if e != nil {
					t.Fatal(e)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".install-bootstrap.") {
						t.Fatal("normal bootstrap cleanup left scratch")
					}
				}
			}
		})
	}
}

func TestInstallerCapabilityChild(t *testing.T) {
	if os.Getenv("TMAG_INSTALLER_CAPABILITY_CHILD") != "1" {
		return
	}
	handled, err := RunChild([]string{"__install-capabilities"})
	if !handled || err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestInstallerCapabilityProbeIsReadOnly(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstallerCapabilityChild$")
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "TMAG_INSTALLER_CAPABILITY_CHILD=1", "TMAG_STATE_DIR="+filepath.Join(root, "state"), "TMAG_AUTH_DIR="+filepath.Join(root, "auth"), "TMUX_PLUGIN_MANAGER_PATH="+filepath.Join(root, "plugins"))
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "tmag/install/v1\n" {
		t.Fatalf("read-only capability: %v %q", err, out)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("capability touched filesystem: %v %v", entries, err)
	}
}
