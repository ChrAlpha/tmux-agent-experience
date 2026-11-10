package binpath

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestPluginCandidates(t *testing.T) {
	got := PluginCandidates(env(map[string]string{"HOME": "/h", "TMUX_PLUGIN_MANAGER_PATH": "/h/.tmux/plugins/"}))
	want := []string{"/h/.tmux/plugins/tmux-agent-experience/bin/tmag", "/h/.config/tmux/plugins/tmux-agent-experience/bin/tmag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got = PluginCandidates(env(map[string]string{"HOME": "/h", "TMUX_PLUGIN_MANAGER_PATH": "plugins", "XDG_CONFIG_HOME": "rel"}))
	if !reflect.DeepEqual(got, []string{"/h/.tmux/plugins/tmux-agent-experience/bin/tmag"}) {
		t.Fatalf("relative roots accepted: %v", got)
	}
}

// The shell command and the Go resolution must pick the same binary.
func TestHookCommandMatchesCandidates(t *testing.T) {
	home := t.TempDir()
	tpm := filepath.Join(t.TempDir(), "plugins")
	marker := filepath.Join(t.TempDir(), "ran")
	install := func(root string) string {
		p := filepath.Join(root, PluginDir, "bin", "tmag")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\nprintf '%s %s' \"$0\" \"$*\" > " + marker + "\ncat >/dev/null\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(vars map[string]string) string {
		t.Helper()
		os.Remove(marker)
		cmd := exec.Command("/bin/sh", "-c", HookCommand("claude"))
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		for k, v := range vars {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin = strings.NewReader(`{"x":1}`)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil || out.Len() != 0 {
			t.Fatalf("hook command: err=%v output=%q", err, out.String())
		}
		b, _ := os.ReadFile(marker)
		return string(b)
	}

	if got := run(map[string]string{"HOME": home}); got != "" {
		t.Fatalf("ran %q with nothing installed", got)
	}
	fallback := install(filepath.Join(home, ".config", "tmux", "plugins"))
	primary := install(tpm)
	vars := map[string]string{"HOME": home, "TMUX_PLUGIN_MANAGER_PATH": tpm + "/"}
	if got, want := run(vars), primary+" hook claude"; got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
	if first := PluginCandidates(env(vars))[0]; first != primary {
		t.Fatalf("Go picks %s, shell picks %s", first, primary)
	}
	delete(vars, "TMUX_PLUGIN_MANAGER_PATH")
	if got, want := run(vars), fallback+" hook claude"; got != want {
		t.Fatalf("fallback ran %q, want %q", got, want)
	}
	// A relative plugin path must not run a file from the working directory.
	cwd := t.TempDir()
	install(filepath.Join(cwd, "rel"))
	cmd := exec.Command("/bin/sh", "-c", HookCommand("claude"))
	cmd.Dir = cwd
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "TMUX_PLUGIN_MANAGER_PATH=rel"}
	os.Remove(marker)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(marker); len(b) != 0 {
		t.Fatalf("relative TMUX_PLUGIN_MANAGER_PATH executed %s", b)
	}
}

// The tmag-peers plugins ship Launcher as their tmag, for the agent's tools.
func TestShippedLaunchers(t *testing.T) {
	for _, path := range []string{
		"plugins/claude/tmag-peers/bin/tmag",
		"plugins/codex/tmag-peers/skills/peers/scripts/tmag",
		"plugins/pi/tmag-peers/skills/peers/scripts/tmag",
		"plugins/omp/tmag-peers/skills/peers/scripts/tmag",
	} {
		full := filepath.Join("..", "..", path)
		b, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != Launcher() {
			t.Errorf("%s differs from binpath.Launcher(); regenerate it", path)
		}
		if st, err := os.Stat(full); err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", path)
		}
	}
}

// The launcher runs what the hooks run: the plugin's binary under the first
// root that has one, else tmag on PATH, never itself; and fails as a missing
// command when there is none.
func TestLauncherRunsTheHooksBinary(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "launcher", "tmag")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte(Launcher()), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := func(path, name string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+name+" \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(vars ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(launcher, "send", "a b")
		cmd.Env = append([]string{"HOME=" + filepath.Join(dir, "home")}, vars...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		code := 0
		var exit *exec.ExitError
		if err := cmd.Run(); errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String()), code
	}
	path := "PATH=" + filepath.Dir(launcher) + ":" + filepath.Join(dir, "other") + ":/usr/bin:/bin"

	if out, code := run(path); code != 127 || !strings.Contains(out, "not installed") {
		t.Fatalf("with no binary: %d %q", code, out)
	}
	fake(filepath.Join(dir, "other", "tmag"), "path")
	if out, _ := run(path); out != "path send a b" {
		t.Fatalf("PATH fallback ran %q", out)
	}
	fake(filepath.Join(dir, "home", ".config", "tmux", "plugins", PluginDir, "bin", "tmag"), "xdg")
	tpm := filepath.Join(dir, "tpm")
	fake(filepath.Join(tpm, PluginDir, "bin", "tmag"), "tpm")
	if out, _ := run(path, "TMUX_PLUGIN_MANAGER_PATH="+tpm+"/"); out != "tpm send a b" {
		t.Fatalf("with TPM ran %q", out)
	}
	if out, _ := run(path); out != "xdg send a b" {
		t.Fatalf("without TMUX_PLUGIN_MANAGER_PATH ran %q", out)
	}
}

// A hook command whose PATH holds the launcher, as an agent's may, reaches
// it when no plugin binary exists; the hook still fails open, and runs a
// tmag found further down PATH.
func TestHookThroughLauncherFailsOpen(t *testing.T) {
	dir := t.TempDir()
	launcherDir := filepath.Join(dir, "launcher")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launcherDir, "tmag"), []byte(Launcher()), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	path := "PATH=" + launcherDir + ":" + other + ":/usr/bin:/bin"
	run := func(command string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Env = []string{"HOME=" + filepath.Join(dir, "home"), path}
		cmd.Stdin = strings.NewReader(`{"x":1}`)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	for _, command := range []string{HookCommand("claude"), AnswerHookCommand("claude")} {
		if out, err := run(command); err != nil || out != "" {
			t.Fatalf("with nothing installed: err=%v output=%q", err, out)
		}
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "tmag"), []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(HookCommand("codex")); err != nil || out != "ran hook codex" {
		t.Fatalf("with tmag further down PATH: err=%v output=%q", err, out)
	}
}
