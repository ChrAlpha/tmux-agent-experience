package installer

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

type processState struct {
	group int
	state string
}

func processStates(t *testing.T) map[int]processState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "pgid=", "-o", "state=")
	var output processOutput
	cmd.Stdout = &output
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	states := make(map[int]processState)
	for _, line := range strings.Split(output.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		id, e1 := strconv.Atoi(fields[0])
		group, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil {
			t.Fatalf("invalid numeric process row")
		}
		states[id] = processState{group: group, state: fields[2]}
	}
	return states
}

// The anchor is a direct child kept alive in its own group until cleanup. We
// signal that group BEFORE Wait reaps its PID, so even failure cannot recycle
// the group identity and signal the developer/test runner's process group.
func protectedScript(t *testing.T, w scriptWorld, args []string, extra ...string) (<-chan string, int) {
	t.Helper()
	fixture := `script=$1; shift; "$script" "$@"; status=$?; printf '%s\n' "$status"; IFS= read -r release`
	argv := append([]string{"-c", fixture, "installer-safety", w.script}, args...)
	cmd := exec.Command("bash", argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PATH="+w.tools+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+w.scratch)
	cmd.Env = append(cmd.Env, extra...)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = input.Close(); _ = cmd.Wait() })
	result := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		if scanner.Scan() {
			result <- scanner.Text()
		} else {
			result <- "no-complete-result"
		}
	}()
	return result, cmd.Process.Pid
}

func TestInstallerRejectsPrivateModesBeforeWatchdogs(t *testing.T) {
	for _, args := range [][]string{{"__worker"}, {"__worker", "1.2.3", "42"}, {"__probe"}, {"__probe", "1", "true"}, {}, {"bad-version"}, {"1.2.3", "extra"}} {
		t.Run(fmt.Sprintf("%q", args), func(t *testing.T) {
			w := newScriptWorld(t)
			result, anchor := protectedScript(t, w, args)
			select {
			case status := <-result:
				if status != "2" {
					t.Fatalf("invalid entry status = %s", status)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("invalid private entry did not return")
			}
			for pid, st := range processStates(t) {
				if st.group == anchor && pid != anchor {
					t.Fatalf("invalid entry armed a descendant: pid=%d state=%s", pid, st.state)
				}
			}
		})
	}
}

func TestInstallerNearCutoffProbeStopsEntireCohort(t *testing.T) {
	w := newScriptWorld(t)
	marker := filepath.Join(w.root, "probe-pids")
	candidate := []byte("#!/usr/bin/env bash\nsleep 30 &\nchild=$!\nprintf '%s %s\\n' \"$$\" \"$child\" >\"$TMAG_TEST_PROBE_PIDS\"\nwait \"$child\"\n")
	archive := releaseArchive(t, candidate, tar.TypeReg)
	asset := fmt.Sprintf("tmag_%s_%s_%s.tar.gz", version.Version, runtime.GOOS, runtime.GOARCH)
	archivePath, checksumPath := filepath.Join(w.root, "fixture.tar.gz"), filepath.Join(w.root, "fixture-checksums")
	writeFixture(t, archivePath, archive, 0o600)
	writeFixture(t, checksumPath, []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset)), 0o600)
	writeFixture(t, filepath.Join(w.tools, "curl"), []byte(`#!/usr/bin/env bash
while [ "$#" -gt 0 ]; do
  case "$1" in -o) target=$2; shift 2 ;; --) url=$2; break ;; *) shift ;; esac
done
case "$url" in
  */checksums.txt) cat "$TMAG_TEST_CHECKSUMS" >"$target" ;;
  *) sleep 0.5; cat "$TMAG_TEST_ARCHIVE" >"$target" ;;
esac
`), 0o755)
	writeFixture(t, filepath.Join(w.root, "bin/tmag"), []byte("#!/bin/sh\necho old\n"), 0o755)
	start := time.Now()
	result, _ := protectedScript(t, w, []string{version.Version}, "TMAG_TEST_BUDGET=2", "TMAG_TEST_RELEASE=https://synthetic.invalid/release", "TMAG_TEST_ARCHIVE="+archivePath, "TMAG_TEST_CHECKSUMS="+checksumPath, "TMAG_TEST_PROBE_PIDS="+marker)
	select {
	case status := <-result:
		if status == "0" {
			t.Fatal("hanging probe returned success")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("whole cohort escaped the attempt deadline")
	}
	elapsed := time.Since(start)
	pids := strings.Fields(string(read(t, marker)))
	if len(pids) != 2 {
		t.Fatalf("probe barrier missing: %q", pids)
	}
	states := processStates(t)
	for _, raw := range pids {
		pid, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := states[pid]; ok && !strings.HasPrefix(st.state, "Z") && !strings.HasPrefix(st.state, "X") {
			t.Fatalf("probe survived caller exit: pid=%d state=%s", pid, st.state)
		}
	}
	if elapsed > 2250*time.Millisecond {
		t.Fatalf("late probe extended total budget: %v", elapsed)
	}
	if string(read(t, filepath.Join(w.root, "bin/tmag"))) != "#!/bin/sh\necho old\n" {
		t.Fatal("prepublication timeout changed target")
	}
}
