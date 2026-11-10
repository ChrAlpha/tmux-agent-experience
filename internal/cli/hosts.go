package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ChrAlpha/tmux-agent-experience/internal/binpath"
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// Identifiers shipped by this repository.
const (
	repo        = binpath.Repo
	marketplace = "tmux-agent-experience"
	pluginName  = "tmag"       // the hooks
	peersName   = "tmag-peers" // the skill for working with other agents
	miseTool    = "github:" + repo
)

// pluginID names a plugin of tmag's marketplace in a host's plugin commands.
func pluginID(name string) string { return name + "@" + marketplace }

// peersOption turns the tmag-peers plugin off: tmag setup installs it unless
// a tmux server sets this to off.
const peersOption = "@tmag-peers"

// peersWanted reports whether tmag-peers belongs in the agents: on unless any
// tmux server tmag finds says off, as for @tmag-answer.
func peersWanted(ctx context.Context) bool {
	for _, srv := range tmux.Servers() {
		if srv.GlobalOption(ctx, peersOption) == "off" {
			return false
		}
	}
	return true
}

// run retains setup's per-command limit and an earlier diagnostic parent.
// The owned helper contains descendants and pipe drain on every exit.
func run(parent context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	cmd, cleanup := helper.Command(ctx, name, args...)
	defer cleanup()
	out := &hostOutput{}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if out.truncated {
		return "", errors.New("host output exceeds bound")
	}
	return strings.TrimSpace(out.String()), nil
}

type hostOutput struct {
	bytes.Buffer
	truncated bool
}

func (o *hostOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, (1<<20)-o.Len())
	o.Buffer.Write(p[:keep])
	o.truncated = o.truncated || keep < n
	return n, nil
}

func hostMetadata(s string, limit int) string {
	if len(s) > limit || strings.ContainsFunc(s, unicode.IsControl) {
		return "unknown"
	}
	return s
}

// plugin is one of tmag's plugins in a host.
type plugin struct {
	Installed bool
	Enabled   bool
	Version   string
	// Elsewhere are copies Pi or oh-my-pi installed from another directory
	// than the tmux plugin's: what removes them (Pi's package paths,
	// oh-my-pi's plugin name).
	Elsewhere []string
}

// outdated reports an installed plugin of another version than the binary.
func (p plugin) outdated() bool {
	return p.Installed && p.Version != "" && p.Version != version.Version
}

// host describes one agent CLI and tmag's plugins in it.
type host struct {
	Name    string // display name
	CLI     string // the agent (proc.Agents), whose command this is
	Present bool   // the CLI is on PATH
	Version string // the CLI's version line
	Listed  bool   // the plugin list could be read
	Tmag    plugin // the hooks, or the extension that runs them
	Peers   plugin // the skill
	// Packages is where Pi or oh-my-pi install tmag's plugins from: their
	// directory in the tmux plugin whose tmag agent hooks run, so that
	// they come and go with it; "" when hooks run a tmag of no plugin.
	Packages string
	// Codex: config.toml records an approval of the plugin's hooks. Codex
	// honours it only while it matches the hashes of the current hooks, which
	// it does not expose, so this is no proof that the hooks run.
	HookApproval    bool
	Unavailable     bool
	ApprovalUnknown bool
}

// of returns the host's record of the named plugin.
func (h *host) of(name string) *plugin {
	if name == peersName {
		return &h.Peers
	}
	return &h.Tmag
}

func claudeHostContext(ctx context.Context) host {
	h := host{Name: "Claude Code", CLI: "claude"}
	h.Version, h.Present, h.Unavailable = probe(ctx, "claude")
	if !h.Present || h.Unavailable {
		return h
	}
	out, err := run(ctx, "claude", "plugin", "list", "--json")
	var plugins []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Enabled bool   `json:"enabled"`
	}
	if err != nil || json.Unmarshal([]byte(out), &plugins) != nil {
		h.Unavailable = true
		return h
	}
	h.Listed = true
	for _, p := range plugins {
		if ctx.Err() != nil {
			h.Unavailable = true
			h.Listed = false
			return h
		}
		for _, name := range []string{pluginName, peersName} {
			if p.ID == pluginID(name) {
				*h.of(name) = plugin{Installed: true, Enabled: p.Enabled, Version: hostMetadata(p.Version, 64)}
			}
		}
	}
	return h
}

// hostProbes probe the host of each agent tmag has plugins for.
var hostProbes = map[string]func(context.Context) host{
	proc.Claude: claudeHostContext, proc.Codex: codexHostContext, proc.Pi: piHostContext, proc.OMP: ompHostContext,
}

// probeHosts probes the hosts, all of them or only the one named, at once,
// and returns them in proc.Agents order.
func probeHosts(ctx context.Context, only string) []host {
	hosts := make([]host, len(proc.Agents))
	var wg sync.WaitGroup
	for i, agent := range proc.Agents {
		if only == "" || agent == only {
			wg.Go(func() { hosts[i] = hostProbes[agent](ctx) })
		}
	}
	wg.Wait()
	return slices.DeleteFunc(hosts, func(h host) bool { return h.CLI == "" })
}

func codexHostContext(ctx context.Context) host {
	h := host{Name: "Codex", CLI: "codex"}
	h.Version, h.Present, h.Unavailable = probe(ctx, "codex")
	if !h.Present || h.Unavailable {
		return h
	}
	out, err := run(ctx, "codex", "plugin", "list", "--json")
	var doc any
	if err != nil || json.Unmarshal([]byte(out), &doc) != nil {
		h.Unavailable = true
		return h
	}
	h.Listed = true
	walkJSON(ctx, doc, func(m map[string]any) {
		name, _ := m["name"].(string)
		if (name != pluginName && name != peersName) || m["installed"] != true {
			return
		}
		p := h.of(name)
		p.Installed = true
		p.Enabled = p.Enabled || m["enabled"] == true
		if v, ok := m["version"].(string); ok {
			p.Version = hostMetadata(v, 64)
		}
	})
	if ctx.Err() != nil {
		h.Unavailable = true
		h.Listed = false
		return h
	}
	h.HookApproval, err = codexHookApproval(ctx)
	h.ApprovalUnknown = err != nil
	return h
}

func probe(ctx context.Context, cli string) (string, bool, bool) {
	if ctx.Err() != nil {
		return "", false, true
	}
	path, err := exec.LookPath(cli)
	if errors.Is(err, exec.ErrNotFound) && ctx.Err() == nil {
		return "", false, false
	}
	if err != nil || ctx.Err() != nil {
		return "", false, true
	}
	v, err := run(ctx, path, "--version")
	if err != nil || v == "" || hostMetadata(v, 128) == "unknown" {
		return "", true, true
	}
	return v, true, false
}

func walkJSON(ctx context.Context, v any, fn func(map[string]any)) {
	if ctx.Err() != nil {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, c := range t {
			walkJSON(ctx, c, fn)
		}
	case []any:
		for _, c := range t {
			walkJSON(ctx, c, fn)
		}
	}
}

// codexHookApproval looks for approved hook-state entries of the plugin in
// config.toml. Only the table headers and their trust lines are inspected.
func codexHookApproval(ctx context.Context) (bool, error) {
	data, err := contextio.ReadFile(ctx, filepath.Join(codexHome(), "config.toml"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	header := []byte(`[hooks.state."` + pluginID(pluginName) + `:`)
	inTable, trusted := false, false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		line := bytes.TrimSpace(sc.Bytes())
		if bytes.HasPrefix(line, []byte("[")) {
			inTable = bytes.HasPrefix(line, header)
			continue
		}
		if inTable && bytes.HasPrefix(line, []byte("trusted_hash")) {
			trusted = true
		}
		if inTable && bytes.Equal(bytes.ReplaceAll(line, []byte(" "), nil), []byte("enabled=false")) {
			return false, nil
		}
	}
	return trusted, errors.Join(sc.Err(), ctx.Err())
}

// codexDaemon finds Codex's managed app-server daemon.
// codexHome is $CODEX_HOME or ~/.codex.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func codexDaemon() (int, bool) {
	pid, found, _ := codexDaemonContext(context.Background())
	return pid, found
}
func codexDaemonContext(ctx context.Context) (int, bool, error) {
	pt, err := proc.SnapshotContext(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, p := range pt.All() {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		if proc.IsCodexService(p) && strings.Contains(p.Args, "--managed-daemon") {
			return p.PID, true, nil
		}
	}
	return 0, false, ctx.Err()
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
