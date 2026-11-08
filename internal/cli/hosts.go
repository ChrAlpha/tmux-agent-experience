package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

const hostOutputLimit = 1 << 20

var errHostOutput = errors.New("host probe output exceeds diagnostic bound")

// probeOutput caps retained stdout before allocation. Returning the input size
// keeps draining under the owned helper's deadline without growing the buffer.
type probeOutput struct {
	data     []byte
	limit    int
	overflow bool
}

func (w *probeOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, w.limit-len(w.data))
	w.data = append(w.data, p[:keep]...)
	w.overflow = w.overflow || keep < n
	return n, nil
}

// runContext retains setup's per-command 20s limit while respecting an earlier
// diagnostic parent. The owned group and pipe drain are contained on every exit.
func runContext(parent context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd, cleanup := helper.Command(ctx, name, args...)
	defer cleanup()
	out := &probeOutput{limit: hostOutputLimit}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	if out.overflow {
		return "", errHostOutput
	}
	return strings.TrimSpace(string(out.data)), nil
}

// displayMetadata prevents oversized or control-bearing CLI metadata from
// becoming diagnostic output. The command's raw stderr is never retained.
func displayMetadata(s string, limit int) string {
	if len(s) > limit || strings.ContainsFunc(s, unicode.IsControl) {
		return "unavailable"
	}
	return s
}
func probeFailure(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout_or_canceled"
	}
	if errors.Is(err, errHostOutput) {
		return "output_capacity"
	}
	return "unavailable"
}

// plugin is one of tmag's plugins in a host.
type plugin struct {
	Installed bool
	Enabled   bool
	Version   string
}

// outdated reports an installed plugin of another version than the binary.
func (p plugin) outdated() bool {
	return p.Installed && p.Version != "" && p.Version != version.Version
}

// host describes one agent CLI and tmag's plugins in it.
type host struct {
	Name    string // display name
	CLI     string // claude | codex
	Present bool   // the CLI is on PATH
	Version string // the CLI's version line
	Listed  bool   // the plugin list could be read
	Tmag    plugin // the hooks
	Peers   plugin // the skill
	// Codex: config.toml records an approval of the plugin's hooks. Codex
	// honours it only while it matches the hashes of the current hooks, which
	// it does not expose, so this is no proof that the hooks run.
	HookApproval    bool
	ProbeFailure    string // fixed failure category; never raw CLI output
	ApprovalUnknown bool
}

// of returns the host's record of the named plugin.
func (h *host) of(name string) *plugin {
	if name == peersName {
		return &h.Peers
	}
	return &h.Tmag
}

func claudeHost() host { return claudeHostContext(context.Background()) }
func claudeHostContext(ctx context.Context) host {
	h := host{Name: "Claude Code", CLI: "claude"}
	h.Version, h.Present, h.ProbeFailure = probeContext(ctx, "claude")
	if !h.Present || h.ProbeFailure != "" {
		return h
	}
	out, err := runContext(ctx, "claude", "plugin", "list", "--json")
	var plugins []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Enabled bool   `json:"enabled"`
	}
	if err != nil {
		h.ProbeFailure = probeFailure(err)
		return h
	}
	if json.Unmarshal([]byte(out), &plugins) != nil {
		h.ProbeFailure = "invalid_plugin_metadata"
		return h
	}
	h.Listed = true
	for _, p := range plugins {
		if err := ctx.Err(); err != nil {
			h.Listed = false
			h.ProbeFailure = probeFailure(err)
			return h
		}
		for _, name := range []string{pluginName, peersName} {
			if p.ID == pluginID(name) {
				*h.of(name) = plugin{Installed: true, Enabled: p.Enabled, Version: displayMetadata(p.Version, 64)}
			}
		}
	}
	return h
}

func codexHost() host { return codexHostContext(context.Background()) }
func codexHostContext(ctx context.Context) host {
	h := host{Name: "Codex", CLI: "codex"}
	h.Version, h.Present, h.ProbeFailure = probeContext(ctx, "codex")
	if !h.Present || h.ProbeFailure != "" {
		return h
	}
	out, err := runContext(ctx, "codex", "plugin", "list", "--json")
	var doc any
	if err != nil {
		h.ProbeFailure = probeFailure(err)
		return h
	}
	if json.Unmarshal([]byte(out), &doc) != nil {
		h.ProbeFailure = "invalid_plugin_metadata"
		return h
	}
	h.Listed = true
	err = walkJSONContext(ctx, doc, func(m map[string]any) {
		name, _ := m["name"].(string)
		if (name != pluginName && name != peersName) || m["installed"] != true {
			return
		}
		p := h.of(name)
		p.Installed = true
		p.Enabled = p.Enabled || m["enabled"] == true
		if v, ok := m["version"].(string); ok {
			p.Version = displayMetadata(v, 64)
		}
	})
	if err != nil {
		h.Listed = false
		h.ProbeFailure = probeFailure(err)
		return h
	}
	h.HookApproval, err = codexHookApprovalContext(ctx)
	h.ApprovalUnknown = err != nil
	return h
}

func probeContext(ctx context.Context, cli string) (versionLine string, present bool, failure string) {
	if err := ctx.Err(); err != nil {
		return "", false, probeFailure(err)
	}
	path, err := exec.LookPath(cli)
	if ctx.Err() != nil {
		return "", false, probeFailure(ctx.Err())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", false, ""
	}
	if err != nil {
		return "", false, "unavailable"
	}
	v, err := runContext(ctx, path, "--version")
	if err != nil {
		return "", true, probeFailure(err)
	}
	if v == "" || displayMetadata(v, 128) == "unavailable" {
		return "", true, "invalid_version_metadata"
	}
	return v, true, ""
}

func walkJSONContext(ctx context.Context, v any, fn func(map[string]any)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, c := range t {
			if err := walkJSONContext(ctx, c, fn); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range t {
			if err := walkJSONContext(ctx, c, fn); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// codexHookApproval looks for approved hook-state entries of the plugin in
// config.toml. Only the table headers and their trust lines are inspected.
func codexHookApprovalContext(ctx context.Context) (bool, error) {
	data, err := contextio.ReadFile(ctx, filepath.Join(codexHome(), "config.toml"), hostOutputLimit)
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
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if proc.IsCodexService(p) && strings.Contains(p.Args, "--managed-daemon") {
			return p.PID, true, nil
		}
	}
	return 0, false, ctx.Err()
}

// hookBinaries lists, in the order agent hooks try them, every tmag a process
// with this environment would find: the tmux plugin's binary, then PATH.
func hookBinariesContext(ctx context.Context, getenv binpath.Getenv) ([]string, error) {
	var found []string
	paths := binpath.PluginCandidates(getenv)
	dirs := filepath.SplitList(getenv("PATH"))
	if len(dirs) > 128 {
		return nil, fmt.Errorf("hook path probe capacity")
	}
	for _, dir := range dirs {
		if dir != "" {
			paths = append(paths, filepath.Join(dir, "tmag"))
		}
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			found = append(found, p)
		}
	}
	return found, ctx.Err()
}

// inPluginDir reports whether p is the tmux plugin's managed binary.
func inPluginDir(p string) bool {
	return strings.HasSuffix(filepath.ToSlash(p), "/"+binpath.PluginDir+"/bin/tmag")
}
