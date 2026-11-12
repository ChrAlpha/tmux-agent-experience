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
	"plugins/claude/tmag/.claude-plugin/plugin.json", "plugins/claude/tmag/hooks/hooks.json",
	"plugins/codex/tmag/.codex-plugin/plugin.json", "plugins/codex/tmag/hooks/hooks.json",
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
	if got := h.tmux("show-option", "-gqv", "@tmag-peers"); got != "on" {
		t.Errorf("@tmag-peers = %q", got)
	}
	if got := h.tmux("show-option", "-gqv", "@tmag-plugin-version"); got != version.Version {
		t.Errorf("@tmag-plugin-version = %q", got)
	}

	// The pane status shows a live agent and hides it once it is gone.
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, "sh", "-c", filepath.Join(bin, "claude")+"; exec sleep 600")
	h.waitPane(pane, "idle", "process")
	status := func() string { return h.tmux("display-message", "-p", "-t", pane, "#{E:@tmag-pane-status}") }
	if got := status(); got != "claude idle" {
		t.Fatalf("pane status = %q", got)
	}
	if err := exec.Command("pkill", "-KILL", "-f", "^"+filepath.Join(bin, "claude")+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	h.eventually("stale status to disappear", func() bool { return status() == "" })
	if _, _, ok := h.paneIn(pane); ok {
		t.Fatal("exited agent still listed")
	}
}

func TestPluginDefaultsCanBeTurnedOff(t *testing.T) {
	h := start(t)
	plugin := h.loadPlugin(false, map[string]string{"@tmag-key-picker": "off", "@tmag-key-setup": "off", "@tmag-alert": "off", "@tmag-peers": "off"})
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
	if got := h.tmux("show-option", "-gqv", "@tmag-peers"); got != "off" {
		t.Errorf("@tmag-peers = %q", got)
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
	h.waitPane(pane, "idle", "process")
	flag := func() string { return h.tmux("display-message", "-p", "-t", pane, "#{window_bell_flag}") }
	if got := flag(); got != "0" {
		t.Fatalf("bell flag before = %s", got)
	}
	h.tmux("send-keys", "-t", pane, "block", "Enter")
	h.waitPane(pane, "blocked", "process")
	h.eventually("bell flag", func() bool { return flag() == "1" })
}

// tmag changes no tmux option: doctor names those that silently undo what it
// shows, and the line that restores each.
func TestDoctorNamesTmuxOptionsThatUndoAlerts(t *testing.T) {
	h := start(t)
	h.tmux("set-option", "-g", "@tmag-alert", "blocked,finished")
	if r := h.tmag("", "doctor"); !strings.Contains(r.out, "✓ options") {
		t.Fatalf("doctor with tmux's defaults: %s", r.out)
	}
	h.tmux("set-option", "-g", "monitor-bell", "off")
	if r := h.tmag("", "doctor"); !strings.Contains(r.out, "! options  monitor-bell is off on e2e, so alerts flag no window: set -g monitor-bell on, or set -g @tmag-alert off") {
		t.Fatalf("doctor with monitor-bell off: %s", r.out)
	}
	h.tmux("set-option", "-g", "@tmag-alert", "off")
	if r := h.tmag("", "doctor"); !strings.Contains(r.out, "✓ options") {
		t.Fatalf("doctor with alerts off: %s", r.out)
	}
}

func TestJumpAndStatus(t *testing.T) {
	h := start(t)
	sock := h.tmux("display-message", "-p", "#{socket_path}")
	blocked := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	finished := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(finished, "idle", "process")
	h.waitPane(blocked, "idle", "process")
	// A turn ends in a pane nobody watches: sidB is finished, not settled.
	if r := h.tmag("", "send", sidB[:8], "--wait", "work"); r.code != 0 {
		t.Fatalf("send to the idle pane: %d %s", r.code, r.out)
	}
	h.tmux("send-keys", "-t", blocked, "block", "Enter")
	h.waitPane(blocked, "blocked", "process")
	s, _ := h.sessionOf(sidB)
	if s.Seen != "" || s.Turn == "" {
		t.Fatalf("the finished turn is %+v", s)
	}

	if r := h.tmag("", "jump", "--server", sock); r.code != 0 || strings.TrimSpace(r.out) != sock+":"+blocked {
		t.Fatalf("jump = %d %q, want %s", r.code, r.out, blocked)
	}
	if r := h.tmag("", "jump", "--server", sock, "--from", blocked); strings.TrimSpace(r.out) != sock+":"+finished {
		t.Fatalf("jump from the blocked pane = %q, want %s", r.out, finished)
	}
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 blocked · 1 finished" {
		t.Fatalf("status = %q", r.out)
	}
	// The person marks the finished turn seen, as a command does.
	if r := h.tmag("", "seen", sidB[:8], "--turn", s.Turn); r.code != 0 {
		t.Fatalf("seen: %d %s", r.code, r.out)
	}
	if got, _ := h.sessionOf(sidB); got.Seen != s.Turn {
		t.Fatalf("after seen: %+v", got)
	}
	if r := h.tmag("", "jump", "--server", sock, "--from", blocked); r.code == 0 {
		t.Fatalf("a settled session is still a jump target: %s", r.out)
	}
	if r := h.tmag("", "status", "--server", sock); strings.TrimSpace(r.out) != "1 blocked · 1 idle" {
		t.Fatalf("status after seen = %q", r.out)
	}
}

func TestSetupInstallsAgentPlugins(t *testing.T) {
	h := start(t)
	log := filepath.Join(h.dir, "plugin-commands")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PLUGIN_LOG", log)
	// Pi and oh-my-pi install tmag's packages from the tmux plugin whose
	// tmag the hooks run; Pi lists them in its settings.
	tpm := filepath.Join(h.dir, "tpm")
	plugin := filepath.Join(tpm, "tmux-agent-experience")
	copyFile(t, filepath.Join(bin, "tmag"), filepath.Join(plugin, "bin", "tmag"))
	packages := map[string]string{}
	for _, agent := range []string{"pi", "omp"} {
		for _, name := range []string{"tmag", "tmag-peers"} {
			f := filepath.Join("plugins", agent, name, "package.json")
			copyFile(t, filepath.Join("..", "..", f), filepath.Join(plugin, f))
			packages[agent+" "+name] = filepath.Join(plugin, "plugins", agent, name)
		}
	}
	t.Setenv("TMUX_PLUGIN_MANAGER_PATH", tpm)
	piDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", piDir)
	piInstalled := func(names ...string) {
		t.Helper()
		var paths []string
		for _, name := range names {
			paths = append(paths, `"`+packages["pi "+name]+`"`)
		}
		if err := os.WriteFile(filepath.Join(piDir, "settings.json"), []byte(`{"packages":[`+strings.Join(paths, ",")+`]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ompInstalled := func(names ...string) string {
		var entries []string
		for _, name := range names {
			entries = append(entries, fmt.Sprintf(`{"name":%q,"version":%q,"path":%q,"enabled":true}`, name, version.Version, packages["omp "+name]))
		}
		return `{"npm":[` + strings.Join(entries, ",") + `]}`
	}

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
		"claude plugin install tmag-peers@tmux-agent-experience",
		"codex plugin marketplace add ChrAlpha/tmux-agent-experience --sparse .agents --sparse plugins/codex",
		"codex plugin add tmag@tmux-agent-experience",
		"codex plugin add tmag-peers@tmux-agent-experience",
		"pi install " + packages["pi tmag"],
		"pi install " + packages["pi tmag-peers"],
		"omp plugin install " + packages["omp tmag"],
		"omp plugin install " + packages["omp tmag-peers"],
	}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("ran:\n%s\nwant:\n%s", got, want)
	}

	// Once every host reports the current plugins, setup has nothing to do.
	entry := `{"id":"%[1]s@tmux-agent-experience","name":"%[1]s","installed":true,"enabled":true,"version":%[2]q}`
	t.Setenv("FAKE_PLUGIN_LIST", "["+fmt.Sprintf(entry, "tmag", version.Version)+","+fmt.Sprintf(entry, "tmag-peers", version.Version)+"]")
	t.Setenv("FAKE_OMP_PLUGIN_LIST", ompInstalled("tmag", "tmag-peers"))
	piInstalled("tmag", "tmag-peers")
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if r := h.tmag("", "setup", "--yes"); r.code != 0 || strings.Count(r.out, "tmag and tmag-peers plugins "+version.Version+" are installed and enabled") != 4 {
		t.Fatalf("setup when current: %d %s", r.code, r.out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("setup ran commands although every plugin is current")
	}

	// With @tmag-peers off, setup takes tmag-peers out of every host, and
	// doctor says so until it has.
	h.tmux("set-option", "-g", "@tmag-peers", "off")
	if r := h.tmag("", "doctor"); !strings.Contains(r.out, "tmag-peers installed although @tmag-peers is off") {
		t.Fatalf("doctor with tmag-peers off but installed: %s", r.out)
	}
	if r := h.tmag("", "setup", "--yes"); r.code != 0 {
		t.Fatalf("setup with tmag-peers off: %d %s", r.code, r.out)
	}
	got, _ = os.ReadFile(log)
	want = "claude plugin uninstall tmag-peers@tmux-agent-experience\ncodex plugin remove tmag-peers@tmux-agent-experience\n" +
		"pi remove " + packages["pi tmag-peers"] + "\nomp plugin uninstall tmag-peers\n"
	if string(got) != want {
		t.Fatalf("ran:\n%s\nwant:\n%s", got, want)
	}
	t.Setenv("FAKE_PLUGIN_LIST", "["+fmt.Sprintf(entry, "tmag", version.Version)+"]")
	t.Setenv("FAKE_OMP_PLUGIN_LIST", ompInstalled("tmag"))
	piInstalled("tmag")
	if r := h.tmag("", "setup", "--yes"); r.code != 0 || strings.Count(r.out, "tmag-peers is off (@tmag-peers)") != 4 {
		t.Fatalf("setup when tmag-peers is gone: %d %s", r.code, r.out)
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
	h.waitPane(pane, "idle", "process")
	h.tmux("send-keys", "-t", pane, "block", "Enter")
	h.waitPane(pane, "blocked", "process")
	if err := exec.Command("pkill", "-KILL", "-o", "-f", "^"+claude+"$").Run(); err != nil {
		t.Fatalf("pkill: %v", err)
	}
	h.waitPane(pane, "", "")
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

// The plugin installs one hook per view change, at an index of its own: a
// reload replaces them in place, a person's hooks keep theirs, and
// @tmag-seen off removes only tmag's.
func TestPluginSeenHooks(t *testing.T) {
	h := start(t)
	h.tmux("set-hook", "-g", "client-attached[0]", "display-message person")
	h.tmux("set-hook", "-gw", "window-pane-changed[97]", "display-message person97")
	plugin := h.loadPlugin(false, nil)
	h.waitBinary(plugin)
	if got := h.tmux("show-option", "-gqv", "@tmag-seen"); got != "on" {
		t.Fatalf("@tmag-seen = %q", got)
	}
	seenHooks := func() map[string]int {
		count := map[string]int{}
		for _, scope := range []string{"-g", "-gw"} {
			for _, line := range strings.Split(h.tmux("show-hooks", scope), "\n") {
				if !strings.Contains(line, "# tmag seen hook") {
					continue
				}
				if name, _, ok := strings.Cut(line, "["); ok {
					count[name]++
				}
			}
		}
		return count
	}
	only := func(want map[string]int) bool {
		got := seenHooks()
		if len(got) != len(want) {
			return false
		}
		for name, n := range want {
			if got[name] != n {
				return false
			}
		}
		return true
	}
	// A pane's change is a window hook; a person's member there moved tmag's
	// to another index, and the session and client hooks stay global.
	if got := h.tmux("show-hooks", "-gw"); !strings.Contains(got, "window-pane-changed[98]") {
		t.Fatalf("the pane hook is not a global window member at its own index:\n%s", got)
	}
	hooks := []string{"session-window-changed", "window-pane-changed", "client-session-changed", "client-attached"}
	one := map[string]int{}
	for _, name := range hooks {
		one[name] = 1
	}
	h.eventually("one hook per view change", func() bool { return only(one) })
	// Loading again replaces them; a person's hook at tmag's index moved
	// tmag's to another one, and stays.
	for range 2 {
		h.tmux("run-shell", filepath.Join(plugin, "tmag.tmux"))
	}
	if got := h.tmux("show-hooks", "-g"); !strings.Contains(got, "client-attached[0] display-message person") {
		t.Fatalf("the person's global hook changed:\n%s", got)
	}
	if got := h.tmux("show-hooks", "-gw"); !strings.Contains(got, "window-pane-changed[97] display-message person97") {
		t.Fatalf("the person's window hook changed:\n%s", got)
	}
	h.eventually("still one hook per view change", func() bool { return only(one) })

	h.tmux("set-option", "-g", "@tmag-seen", "off")
	h.tmux("run-shell", filepath.Join(plugin, "tmag.tmux"))
	h.eventually("no tmag hooks", func() bool { return only(nil) })
	if got := h.tmux("show-hooks", "-g"); !strings.Contains(got, "client-attached[0] display-message person") {
		t.Fatalf("the person's global hook changed with tmag off:\n%s", got)
	}
	if got := h.tmux("show-hooks", "-gw"); !strings.Contains(got, "window-pane-changed[97] display-message person97") {
		t.Fatalf("the person's window hook changed with tmag off:\n%s", got)
	}
}
