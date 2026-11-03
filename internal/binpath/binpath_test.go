package binpath

import (
	"bytes"
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
