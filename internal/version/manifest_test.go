package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/binpath"
)

// Every plugin manifest ships the binary's version.
func TestManifestVersions(t *testing.T) {
	for _, path := range []string{
		"plugins/claude/.claude-plugin/plugin.json",
		"plugins/codex/.codex-plugin/plugin.json",
	} {
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

// The agent plugins run exactly binpath.HookCommand for every hook.
func TestPluginHooksUseHookCommand(t *testing.T) {
	for agent, path := range map[string]string{
		"claude": "plugins/claude/hooks/hooks.json",
		"codex":  "plugins/codex/hooks/hooks.json",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct{ Command string }
			}
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		for event, groups := range doc.Hooks {
			for _, g := range groups {
				for _, h := range g.Hooks {
					if h.Command != binpath.HookCommand(agent) {
						t.Errorf("%s %s: command %q", path, event, h.Command)
					}
				}
			}
		}
	}
}
