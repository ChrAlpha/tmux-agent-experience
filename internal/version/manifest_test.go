package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/binpath"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
)

// manifests are the agent plugins' manifests: the hooks plugin tmag and the
// skill plugin tmag-peers for each host.
var manifests = []string{
	"plugins/claude/tmag/.claude-plugin/plugin.json",
	"plugins/claude/tmag-peers/.claude-plugin/plugin.json",
	"plugins/codex/tmag/.codex-plugin/plugin.json",
	"plugins/codex/tmag-peers/.codex-plugin/plugin.json",
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
// Every timeout leaves the hook its whole hook.Deadline.
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
					Command string
					Timeout int
				}
			}
		}
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
					if h.Command != want || h.Timeout != timeout {
						t.Errorf("%s %s: command %q, timeout %d", path, event, h.Command, h.Timeout)
					}
				}
			}
		}
	}
}
