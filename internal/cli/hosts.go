package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/binpath"
	"github.com/ChrAlpha/tmux-agent-experience/internal/codexidx"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Identifiers shipped by this repository.
const (
	repo        = "ChrAlpha/tmux-agent-experience"
	marketplace = "tmux-agent-experience"
	pluginName  = "tmag"
	pluginID    = pluginName + "@" + marketplace
	miseTool    = "github:" + repo
)

// run executes a command and returns its trimmed stdout.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// host describes one agent CLI and the tmag plugin in it.
type host struct {
	Name      string // display name
	CLI       string // claude | codex
	Present   bool   // the CLI is on PATH
	Version   string // the CLI's version line
	Listed    bool   // the plugin list could be read
	Installed bool
	Enabled   bool
	PluginVer string
	// Codex: config.toml records an approval of the plugin's hooks. Codex
	// honours it only while it matches the hashes of the current hooks, which
	// it does not expose, so this is no proof that the hooks run.
	HookApproval bool
}

func claudeHost() host {
	h := host{Name: "Claude Code", CLI: "claude"}
	if h.Version, h.Present = probe("claude"); !h.Present {
		return h
	}
	out, err := run("claude", "plugin", "list", "--json")
	var plugins []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Enabled bool   `json:"enabled"`
	}
	if err != nil || json.Unmarshal([]byte(out), &plugins) != nil {
		return h
	}
	h.Listed = true
	for _, p := range plugins {
		if p.ID == pluginID {
			h.Installed, h.Enabled, h.PluginVer = true, p.Enabled, p.Version
		}
	}
	return h
}

func codexHost() host {
	h := host{Name: "Codex", CLI: "codex"}
	if h.Version, h.Present = probe("codex"); !h.Present {
		return h
	}
	out, err := run("codex", "plugin", "list", "--json")
	var doc any
	if err != nil || json.Unmarshal([]byte(out), &doc) != nil {
		return h
	}
	h.Listed = true
	walkJSON(doc, func(m map[string]any) {
		if m["name"] != pluginName || m["installed"] != true {
			return
		}
		h.Installed = true
		h.Enabled = h.Enabled || m["enabled"] == true
		if v, ok := m["version"].(string); ok {
			h.PluginVer = v
		}
	})
	h.HookApproval = codexHookApproval()
	return h
}

func probe(cli string) (string, bool) {
	if _, err := exec.LookPath(cli); err != nil {
		return "", false
	}
	v, err := run(cli, "--version")
	return v, err == nil
}

func walkJSON(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, c := range t {
			walkJSON(c, fn)
		}
	case []any:
		for _, c := range t {
			walkJSON(c, fn)
		}
	}
}

// codexHookApproval looks for approved hook-state entries of the plugin in
// config.toml. Only the table headers and their trust lines are inspected.
func codexHookApproval() bool {
	data, err := os.ReadFile(filepath.Join(codexidx.Home(), "config.toml"))
	if err != nil {
		return false
	}
	header := []byte(`[hooks.state."` + pluginID + `:`)
	inTable, trusted := false, false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if bytes.HasPrefix(line, []byte("[")) {
			inTable = bytes.HasPrefix(line, header)
			continue
		}
		if inTable && bytes.HasPrefix(line, []byte("trusted_hash")) {
			trusted = true
		}
		if inTable && bytes.Equal(bytes.ReplaceAll(line, []byte(" "), nil), []byte("enabled=false")) {
			return false
		}
	}
	return trusted
}

// codexDaemon finds Codex's managed app-server daemon.
func codexDaemon() (int, bool) {
	pt, err := proc.Snapshot()
	if err != nil {
		return 0, false
	}
	for _, p := range pt.All() {
		if proc.IsCodexService(p) && strings.Contains(p.Args, "--managed-daemon") {
			return p.PID, true
		}
	}
	return 0, false
}

// hookBinaries lists, in the order agent hooks try them, every tmag a process
// with this environment would find: the tmux plugin's binary, then PATH.
func hookBinaries(getenv binpath.Getenv) []string {
	var found []string
	for _, p := range binpath.PluginCandidates(getenv) {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			found = append(found, p)
		}
	}
	return append(found, proc.LookPathAll(getenv("PATH"), "tmag")...)
}

// inPluginDir reports whether p is the tmux plugin's managed binary.
func inPluginDir(p string) bool {
	return strings.HasSuffix(filepath.ToSlash(p), "/"+binpath.PluginDir+"/bin/tmag")
}
