package version

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/binpath"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
)

// manifests are the agent plugins' manifests: the hooks plugin tmag and the
// skill plugin tmag-peers for each host; Pi's and oh-my-pi's are packages.
var manifests = []string{
	"plugins/claude/tmag/.claude-plugin/plugin.json",
	"plugins/claude/tmag-peers/.claude-plugin/plugin.json",
	"plugins/codex/tmag/.codex-plugin/plugin.json",
	"plugins/codex/tmag-peers/.codex-plugin/plugin.json",
	"plugins/pi/tmag/package.json",
	"plugins/pi/tmag-peers/package.json",
	"plugins/omp/tmag/package.json",
	"plugins/omp/tmag-peers/package.json",
}

// Every plugin manifest ships the binary's version.
func TestManifestVersions(t *testing.T) {
	for _, path := range manifests {
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		var m struct{ Version string }
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if m.Version != Version {
			t.Errorf("%s has version %q, binary is %q", path, m.Version, Version)
		}
	}
}

// Each marketplace lists both plugins of its host, from a directory named
// after the plugin whose manifest carries that name, inside the paths
// tmag setup checks out (plugins/claude, plugins/codex).
func TestMarketplaces(t *testing.T) {
	read := func(path string, v any) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var claude struct {
		Plugins []struct{ Name, Source string }
	}
	read(".claude-plugin/marketplace.json", &claude)
	var codex struct {
		Plugins []struct {
			Name   string
			Source struct{ Path string }
		}
	}
	read(".agents/plugins/marketplace.json", &codex)
	sources := map[string][]string{}
	for _, p := range claude.Plugins {
		sources["claude"] = append(sources["claude"], p.Name+" "+p.Source)
	}
	for _, p := range codex.Plugins {
		sources["codex"] = append(sources["codex"], p.Name+" "+p.Source.Path)
	}
	for host, manifest := range map[string]string{"claude": ".claude-plugin/plugin.json", "codex": ".codex-plugin/plugin.json"} {
		want := []string{"tmag ./plugins/" + host + "/tmag", "tmag-peers ./plugins/" + host + "/tmag-peers"}
		if !reflect.DeepEqual(sources[host], want) {
			t.Errorf("%s marketplace lists %v, want %v", host, sources[host], want)
		}
		for _, name := range []string{"tmag", "tmag-peers"} {
			var m struct{ Name string }
			read(filepath.Join("plugins", host, name, manifest), &m)
			if m.Name != name {
				t.Errorf("plugins/%s/%s is named %q", host, name, m.Name)
			}
		}
	}
}

// The agent plugins run exactly binpath.HookCommand for every hook, except
// Claude Code's PermissionRequest, which runs binpath.AnswerHookCommand.
// Every timeout leaves the hook its whole hook.Deadline. Codex's hooks share
// one statusMessage: Codex's /hooks lists a hook by it, as "Unnamed hook"
// without one, and shows it while a hook runs. Claude Code's have none, which
// keeps its own text while one runs, the permission dialog's answer-hook too.
func TestPluginHooksUseHookCommand(t *testing.T) {
	for agent, path := range map[string]string{
		"claude": "plugins/claude/tmag/hooks/hooks.json",
		"codex":  "plugins/codex/tmag/hooks/hooks.json",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command       string
					Timeout       int
					StatusMessage string
				}
			}
		}
		status := map[string]string{"codex": "Reporting state to tmag"}[agent]
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		for event, groups := range doc.Hooks {
			for _, g := range groups {
				for _, h := range g.Hooks {
					want, timeout := binpath.HookCommand(agent), 10
					switch {
					case agent == "claude" && event == "PermissionRequest":
						// It waits for an answer as long as Claude lets it.
						want, timeout = binpath.AnswerHookCommand(agent), 600
					case agent == "codex" && (event == "SessionEnd" || event == "Interrupt"):
						// Codex 0.166 caps these at 3 s and warns about more.
						timeout = 3
					}
					if time.Duration(timeout)*time.Second < hook.Deadline {
						t.Errorf("%s %s: timeout %d s is shorter than hook.Deadline", path, event, timeout)
					}
					if h.Command != want || h.Timeout != timeout || h.StatusMessage != status {
						t.Errorf("%s %s: command %q, timeout %d, statusMessage %q", path, event, h.Command, h.Timeout, h.StatusMessage)
					}
				}
			}
		}
	}
}

// readRepo reads a file of the repository.
func readRepo(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Pi's and oh-my-pi's plugins are packages named after their directories,
// which their own hosts load (the pi key for Pi, omp for oh-my-pi) from
// files they ship: the tmag extension, and the tmag-peers skill.
func TestPiPackages(t *testing.T) {
	for _, host := range []string{"pi", "omp"} {
		for _, name := range []string{"tmag", "tmag-peers"} {
			dir := filepath.Join("plugins", host, name)
			var pkg map[string]json.RawMessage
			if err := json.Unmarshal(readRepo(t, filepath.Join(dir, "package.json")), &pkg); err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			var got string
			_ = json.Unmarshal(pkg["name"], &got)
			if got != name || string(pkg["private"]) != "true" {
				t.Errorf("%s/package.json names %q, private %s", dir, got, pkg["private"])
			}
			var manifest struct{ Extensions, Skills []string }
			if err := json.Unmarshal(pkg[host], &manifest); err != nil {
				t.Errorf("%s/package.json has no %s manifest: %v", dir, host, err)
			}
			for _, entry := range append(manifest.Extensions, manifest.Skills...) {
				if _, err := os.Stat(filepath.Join("..", "..", dir, entry)); err != nil {
					t.Errorf("%s lists %s: %v", dir, entry, err)
				}
			}
			if other := map[string]string{"pi": "omp", "omp": "pi"}[host]; pkg[other] != nil {
				// oh-my-pi reads a pi manifest where it finds no omp one.
				t.Errorf("%s/package.json also has a %s manifest", dir, other)
			}
		}
	}
}

// Pi's and oh-my-pi's tmag extensions run exactly binpath.HookCommand for
// their agent, through one reporter that both ship byte for byte.
func TestPiExtensionsRunHookCommand(t *testing.T) {
	hookRE := regexp.MustCompile(`(?m)^const hook = (".*");$`)
	for _, agent := range []string{"pi", "omp"} {
		path := filepath.Join("plugins", agent, "tmag", "extensions", "tmag.ts")
		m := hookRE.FindSubmatch(readRepo(t, path))
		var command string
		if m == nil || json.Unmarshal(m[1], &command) != nil {
			t.Fatalf("%s declares no hook command as a JSON string", path)
		}
		if command != binpath.HookCommand(agent) {
			t.Errorf("%s runs %q, want %q", path, command, binpath.HookCommand(agent))
		}
	}
	pi, omp := readRepo(t, "plugins/pi/tmag/extensions/report.ts"), readRepo(t, "plugins/omp/tmag/extensions/report.ts")
	if !bytes.Equal(pi, omp) {
		t.Error("plugins/omp/tmag/extensions/report.ts differs from plugins/pi/tmag/extensions/report.ts")
	}
}
