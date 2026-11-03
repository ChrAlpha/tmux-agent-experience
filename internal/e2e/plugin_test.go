package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// pluginFiles are what TPM's clone provides and tmag.tmux needs.
var pluginFiles = []string{
	"tmag.tmux", "scripts/install.sh", "scripts/tmag",
	"plugins/claude/.claude-plugin/plugin.json", "plugins/claude/hooks/hooks.json",
	"plugins/codex/.codex-plugin/plugin.json", "plugins/codex/hooks/hooks.json",
}

// release builds a local release of the test binary, laid out like GitHub's
// download URLs, and returns its base directory.
func release(t *testing.T, corrupt bool) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "v"+version.Version)
	stage := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(bin, "tmag"), filepath.Join(stage, "tmag"))
	asset := fmt.Sprintf("tmag_%s_%s_%s.tar.gz", version.Version, runtime.GOOS, runtime.GOARCH)
	if out, err := exec.Command("tar", "-C", stage, "-czf", filepath.Join(dir, asset), "tmag").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, asset))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if corrupt {
		sum[0] ^= 0xff
	}
	line := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	st, _ := src.Stat()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// loadPlugin lays the plugin out the way TPM does under a private plugin path,
// sets the given options and runs tmag.tmux. It returns the plugin directory.
func (h *harness) loadPlugin(corrupt bool, options map[string]string) string {
	h.t.Helper()
	root := filepath.Join(h.dir, "tpm")
	plugin := filepath.Join(root, "tmux-agent-experience")
	for _, f := range pluginFiles {
		copyFile(h.t, filepath.Join("..", "..", f), filepath.Join(plugin, f))
	}
	h.tmux("set-environment", "-g", "TMUX_PLUGIN_MANAGER_PATH", root+"/")
	h.tmux("set-option", "-g", "@tmag-release-url", "file://"+release(h.t, corrupt))
	for k, v := range options {
		h.tmux("set-option", "-g", k, v)
	}
	h.tmux("run-shell", filepath.Join(plugin, "tmag.tmux"))
	return plugin
}

func (h *harness) waitBinary(plugin string) {
	h.t.Helper()
	h.eventually("plugin binary", func() bool {
		out, err := exec.Command(filepath.Join(plugin, "bin", "tmag"), "version").Output()
		return err == nil && strings.Contains(string(out), version.Version)
	})
}

func (h *harness) key(key string) string {
	out, _ := exec.Command("tmux", "-L", h.name, "list-keys", "-T", "prefix", key).CombinedOutput()
	return string(out)
}

func TestPluginInstallsBinaryAndSetsUp(t *testing.T) {
	h := start(t)
	h.tmux("set-option", "-g", "status-right", "#{tmag_status} | %H:%M")
	plugin := h.loadPlugin(false, nil)
	h.waitBinary(plugin)

	for key, want := range map[string]string{"a": "choose-tree", "A": "jump", "M-a": "setup"} {
		if got := h.key(key); !strings.Contains(got, want) {
			t.Errorf("prefix %s = %q, want %s", key, got, want)
		}
	}
	right := h.tmux("show-option", "-gqv", "status-right")
	if !strings.Contains(right, "/scripts/tmag' status --tmux --server '#{socket_path}')") || !strings.HasSuffix(right, " | %H:%M") {
		t.Errorf("status-right = %q", right)
	}
	if got := h.tmux("show-option", "-gqv", "@tmag-alert"); got != "blocked,finished" {
		t.Errorf("@tmag-alert = %q", got)
	}
	if got := h.tmux("show-option", "-gqv", "@tmag-plugin-version"); got != version.Version {
		t.Errorf("@tmag-plugin-version = %q", got)
	}

	// The pane status shows a live agent and hides it once it is gone.
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, "sh", "-c", filepath.Join(bin, "claude")+"; exec sleep 600")
	h.waitState(pane, "idle", "process")
	status := func() string { return h.tmux("display-message", "-p", "-t", pane, "#{E:@tmag-pane-status}") }
	if got := status(); got != "claude idle" {
		t.Fatalf("pane status = %q", got)
	}
	if err := exec.Command("pkill", "-KILL", "-f", "^"+filepath.Join(bin, "claude")+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	h.eventually("stale status to disappear", func() bool { return status() == "" })
	if _, ok := h.agentIn(pane); ok {
		t.Fatal("exited agent still listed")
	}
}

func TestPluginDefaultsCanBeTurnedOff(t *testing.T) {
	h := start(t)
	plugin := h.loadPlugin(false, map[string]string{"@tmag-key-picker": "off", "@tmag-key-setup": "off", "@tmag-alert": "off"})
	h.waitBinary(plugin)
	if got := h.key("a"); strings.Contains(got, "choose-tree") {
		t.Errorf("picker bound although off: %q", got)
	}
	if got := h.key("M-a"); strings.Contains(got, "setup") {
		t.Errorf("setup bound although off: %q", got)
	}
	if got := h.key("A"); !strings.Contains(got, "jump") {
		t.Errorf("jump not bound: %q", got)
	}
	if got := h.tmux("show-option", "-gqv", "@tmag-alert"); got != "off" {
		t.Errorf("@tmag-alert = %q", got)
	}
}

func TestPluginRejectsBadChecksum(t *testing.T) {
	h := start(t)
	plugin := h.loadPlugin(true, nil)
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(filepath.Join(plugin, "bin", "tmag")); err == nil {
		t.Fatal("installed a binary whose checksum does not match")
	}
	if _, err := os.Stat(filepath.Join(plugin, "bin", ".install.lock")); err == nil {
		t.Fatal("install lock left behind")
	}
}

func TestBellMarksWaitingWindow(t *testing.T) {
	h := start(t)
	h.waitBinary(h.loadPlugin(false, nil))
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitState(pane, "idle", "process")
	flag := func() string { return h.tmux("display-message", "-p", "-t", pane, "#{window_bell_flag}") }
	if got := flag(); got != "0" {
		t.Fatalf("bell flag before = %s", got)
	}
	h.tmux("send-keys", "-t", pane, "block", "Enter")
	h.waitState(pane, "blocked", "process")
	h.eventually("bell flag", func() bool { return flag() == "1" })
}

func TestJumpAndStatus(t *testing.T) {
	h := start(t)
	sock := h.tmux("display-message", "-p", "#{socket_path}")
	blocked := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	idle := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitState(idle, "idle", "process")
	h.waitState(blocked, "idle", "process")
	h.tmux("send-keys", "-t", blocked, "block", "Enter")
	h.waitState(blocked, "blocked", "process")

	if r := h.tmag("", "jump", "--server", sock); r.code != 0 || strings.TrimSpace(r.out) != sock+":"+blocked {
		t.Fatalf("jump = %d %q, want %s", r.code, r.out, blocked)
	}
	if r := h.tmag("", "jump", "--server", sock, "--from", blocked); strings.TrimSpace(r.out) != sock+":"+idle {
		t.Fatalf("jump from the blocked pane = %q, want %s", r.out, idle)
	}
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 blocked · 1 idle" {
		t.Fatalf("status = %q", r.out)
	}
}

func TestSetupInstallsAgentPlugins(t *testing.T) {
	h := start(t)
	log := filepath.Join(h.dir, "plugin-commands")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PLUGIN_LOG", log)

	// Without --yes and without a terminal, setup only shows its plan.
	if r := h.tmag("", "setup"); r.code != 0 || !strings.Contains(r.out, "tmag setup --yes") {
		t.Fatalf("setup: %d %s", r.code, r.out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("setup changed something without --yes")
	}

	if r := h.tmag("", "setup", "--yes"); r.code != 0 {
		t.Fatalf("setup --yes: %d %s", r.code, r.out)
	}
	got, _ := os.ReadFile(log)
	want := strings.Join([]string{
		"claude plugin marketplace add ChrAlpha/tmux-agent-experience --sparse .claude-plugin plugins/claude",
		"claude plugin install tmag@tmux-agent-experience",
		"codex plugin marketplace add ChrAlpha/tmux-agent-experience --sparse .agents --sparse plugins/codex",
		"codex plugin add tmag@tmux-agent-experience",
	}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("ran:\n%s\nwant:\n%s", got, want)
	}

	// Once both report the current plugin, setup has nothing to do.
	t.Setenv("FAKE_PLUGIN_LIST", fmt.Sprintf(`[{"id":"tmag@tmux-agent-experience","name":"tmag","installed":true,"enabled":true,"version":%q}]`, version.Version))
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if r := h.tmag("", "setup", "--yes"); r.code != 0 || strings.Count(r.out, "installed and enabled") != 2 {
		t.Fatalf("setup when current: %d %s", r.code, r.out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("setup ran commands although both plugins are current")
	}
}

func TestPickerClearsStaleRecords(t *testing.T) {
	h := start(t)
	h.waitBinary(h.loadPlugin(false, nil))
	// Killed while blocked, without SessionEnd; a silent claude follows, so
	// the pane still runs a command of the recorded name.
	claude := filepath.Join(bin, "claude")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"},
		"sh", "-c", claude+"; FAKE_HOOKS=0 FAKE_SESSION= "+claude)
	h.waitState(pane, "idle", "process")
	h.tmux("send-keys", "-t", pane, "block", "Enter")
	h.waitState(pane, "blocked", "process")
	if err := exec.Command("pkill", "-KILL", "-o", "-f", "^"+claude+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	h.waitState(pane, "unknown", "unbound")
	listed := func() string {
		return h.tmux("display-message", "-p", "-t", pane, "#{?#{&&:#{@tmag_state},#{==:#{pane_current_command},#{@tmag_cmd}}},#{@tmag_state},}")
	}
	if got := listed(); got != "blocked" {
		t.Fatalf("precondition: the picker filter sees %q", got)
	}
	// Run the commands bound to the picker key; list-keys escapes their
	// separator for bind-key. Without a client choose-tree fails, after
	// run-shell has finished.
	m := regexp.MustCompile(`(?m)^bind-key\s+-T prefix\s+a\s+(.*)$`).FindStringSubmatch(h.key("a"))
	if m == nil {
		t.Fatalf("picker binding: %q", h.key("a"))
	}
	conf := filepath.Join(h.dir, "picker.conf")
	if err := os.WriteFile(conf, []byte(strings.ReplaceAll(m[1], ` \; `, " ; ")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("tmux", "-L", h.name, "source-file", conf).Run()
	if got := listed(); got != "" {
		t.Fatalf("the picker would list a vanished agent as %q", got)
	}
}
