package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// Minimum supported tmux version.
const minTmuxMajor, minTmuxMinor = 3, 2

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail | skip
	Detail string `json:"detail"`
}

func (a *app) cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("doctor", err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	probeCtx, stopProbe := context.WithTimeout(ctx, doctorProbeBudget)
	operational := a.operationalEvidence(probeCtx)
	checks := []check{checkTmux(probeCtx), checkPlugin(probeCtx), checkBinaryContext(probeCtx), a.checkHookErrors(probeCtx), a.checkNotify(probeCtx), checkServe(probeCtx)}
	peers, peersErr := doctorPeersWanted(probeCtx)
	if peersErr != nil {
		operational.Complete = false
		checks = append(checks, check{"claude", "warn", "agent plugin checks incomplete: peers policy unavailable"}, check{"codex", "warn", "agent plugin checks incomplete: peers policy unavailable"})
	} else {
		claude := claudeHostContext(probeCtx)
		codex := codexHostContext(probeCtx)
		codexResult, daemonComplete := codexCheck(probeCtx, codex, peers)
		checks = append(checks, claudeCheck(claude, peers), codexResult)
		operational.Complete = operational.Complete && claude.ProbeFailure == "" && codex.ProbeFailure == "" && daemonComplete
	}

	operational.Complete = operational.Complete && probeCtx.Err() == nil
	stopProbe()
	checks = append(checks, operationalChecks(operational)...)

	failed := false
	for _, c := range checks {
		failed = failed || c.Status == "fail"
	}
	if a.json {
		a.emit(map[string]any{"version": version.Version, "checks": checks, "operational": operational})
	} else {
		marks := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "-"}
		for _, c := range checks {
			fmt.Fprintf(a.stdout, "%s %-8s %s\n", marks[c.Status], c.Name, c.Detail)
		}
		a.printOperational(operational)
	}
	if failed {
		return ExitError
	}
	return ExitOK
}

func checkTmux(ctx context.Context) check {
	out, err := runContext(ctx, "tmux", "-V")
	if ctx.Err() != nil {
		return check{"tmux", "warn", "version probe incomplete within shared budget"}
	}
	if len(out) > 128 || displayMetadata(out, 128) == "unavailable" {
		return check{"tmux", "warn", "version output exceeds diagnostic bound"}
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return check{"tmux", "fail", "tmux not found"}
		}
		return check{"tmux", "warn", "version probe unavailable: " + probeFailure(err)}
	}
	v := strings.TrimPrefix(out, "tmux ")
	if !tmux.VersionAtLeast(v, minTmuxMajor, minTmuxMinor) {
		return check{"tmux", "fail", fmt.Sprintf("%s; tmag needs %d.%d or newer", v, minTmuxMajor, minTmuxMinor)}
	}
	live := 0
	servers, err := tmux.ServersLimitContext(ctx, policy.MaxServers)
	if err != nil {
		return check{"tmux", "warn", "server discovery incomplete within shared probe budget"}
	}
	for _, s := range servers {
		if _, err := s.Version(ctx); err == nil {
			live++
		}
	}
	if ctx.Err() != nil {
		return check{"tmux", "warn", fmt.Sprintf("server probe incomplete; %d running server(s) observed", live)}
	}
	return check{"tmux", "ok", fmt.Sprintf("%s, %d server(s) running", v, live)}
}

// checkPlugin reports whether the tmux plugin is loaded, and at which version.
func checkPlugin(ctx context.Context) check {
	loaded, stale := 0, false
	servers, err := tmux.ServersLimitContext(ctx, policy.MaxServers)
	if err != nil {
		return check{"plugin", "warn", "server discovery incomplete"}
	}
	for _, s := range servers {
		v, err := s.GlobalOptionContext(ctx, "@tmag-plugin-version")
		if err != nil {
			if absent, probeErr := s.Absent(ctx); probeErr == nil && absent {
				continue
			}
			return check{"plugin", "warn", "plugin option lookup unavailable"}
		}
		switch v {
		case "":
		case version.Version:
			loaded++
		default:
			stale = true
		}
	}
	switch {
	case stale:
		return check{"plugin", "warn", "loaded plugin version differs from binary; reload tmux.conf or update with prefix + U"}
	case loaded == 0:
		return check{"plugin", "warn", "tmux plugin not loaded; add set -g @plugin '" + repo + "' to tmux.conf and press prefix + I"}
	}
	return check{"plugin", "ok", fmt.Sprintf("tmux plugin %s loaded on %d server(s)", version.Version, loaded)}
}

func checkBinaryContext(ctx context.Context) check {
	self, _ := os.Executable()
	found, err := hookBinariesContext(ctx, os.Getenv)
	if err != nil {
		return check{"tmag", "warn", "hook binary discovery unavailable within shared budget"}
	}
	return binaryCheck(self, found)
}

// binaryCheck judges which tmag agent hooks will run, given every tmag they
// would find in order and the running binary self.
func binaryCheck(self string, found []string) check {
	if len(found) == 0 {
		return check{"tmag", "fail", fmt.Sprintf("%s at %s, but agent hooks find no tmag: neither the tmux plugin's bin/tmag nor tmag on PATH", version.Version, self)}
	}
	if shim, ok := viaMiseShim(found); ok {
		return check{"tmag", "ok", fmt.Sprintf("%s via mise shim %s", version.Version, shim)}
	}
	first := found[0]
	if inMiseInstalls(first) {
		return check{"tmag", "warn", fmt.Sprintf("%s is inside mise's versioned install directory; "+
			"long-running agents such as the Codex daemon lose it after an upgrade. Put mise's shims directory on PATH", first)}
	}
	if canonical(first) != canonical(self) {
		return check{"tmag", "warn", fmt.Sprintf("running %s, but hooks will run %s", canonical(self), canonical(first))}
	}
	return check{"tmag", "ok", fmt.Sprintf("%s at %s", version.Version, first)}
}

// viaMiseShim reports whether tmag is reached through a mise shim. When a shim
// runs a tool, mise puts the tool's versioned directory first on PATH, so the
// shim shows up behind it.
func viaMiseShim(found []string) (string, bool) {
	if len(found) == 0 || (!isMiseShim(found[0]) && !inMiseInstalls(found[0])) {
		return "", false
	}
	for _, p := range found {
		if isMiseShim(p) {
			return p, true
		}
	}
	return "", false
}

// isMiseShim reports whether p is a mise shim, a link to the mise binary.
func isMiseShim(p string) bool {
	return filepath.Base(canonical(p)) == "mise"
}

// inMiseInstalls reports whether p lies in a mise data directory's versioned
// installs/ tree, recognised by the shims/ directory next to it.
func inMiseInstalls(p string) bool {
	for dir := filepath.Dir(p); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "installs" {
			st, err := os.Stat(filepath.Join(filepath.Dir(dir), "shims"))
			return err == nil && st.IsDir()
		}
	}
	return false
}

func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func (a *app) checkState(ctx context.Context) check {
	return logHealthCheck(a.log.ProbeHealth(ctx))
}
func logHealthCheck(h eventlog.Health) check {
	status := "warn"
	switch h.State {
	case "readable":
		status = "ok"
	case "integrity_loss", "incomplete_tail", "contended", "canceled":
		status = "fail"
	}
	detail := fmt.Sprintf("event log %s; checked %d bytes/%d complete records; %s", h.State, h.Bytes, h.Records, h.Note)
	if h.File != "" {
		detail += fmt.Sprintf("; %s at byte %d", h.File, h.Offset)
	}
	if h.State == "integrity_loss" || h.State == "incomplete_tail" {
		detail += "; preserve evidence for explicit maintenance"
	}
	return check{"state", status, detail}
}

func (a *app) checkHookErrors(parent context.Context) check {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	data, err := contextio.ReadFile(ctx, filepath.Join(a.log.Dir, "hook.log"), diagnostic.MaxLogBytes)
	if errors.Is(err, os.ErrNotExist) {
		return check{"hooks", "ok", "no hook errors recorded"}
	}
	if err != nil {
		return check{"hooks", "warn", "hook log is unreadable or exceeds its bound"}
	}
	count := 0
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, line := range strings.Split(string(data), "\n") {
		if ctx.Err() != nil {
			return check{"hooks", "warn", "historical hook log probe incomplete within shared budget"}
		}
		var entry struct{ Time time.Time }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Time.After(cutoff) {
			count++
			continue
		}
		ts, _, _ := strings.Cut(line, " ")
		if at, err := time.Parse(time.RFC3339, ts); err == nil && at.After(cutoff) {
			count++
		}
	}
	if count > 0 {
		return check{"hooks", "ok", fmt.Sprintf("%d historical hook failure record(s) in the last 24h; current health is established separately", count)}
	}
	return check{"hooks", "ok", "no historical hook failure records found in the last 24h; recording may be incomplete"}
}

// pluginNote describes tmag's plugins in a host and whether they need action.
func pluginNote(h host, peers bool) (status, note string) {
	if !h.Listed {
		return "warn", "cannot list plugins"
	}
	var notes []string
	status = "ok"
	for _, name := range []string{pluginName, peersName} {
		p := *h.of(name)
		switch {
		case name == peersName && !peers && p.Installed:
			status = "warn"
			notes = append(notes, fmt.Sprintf("%s installed although %s is off; run tmag setup to remove it", name, peersOption))
		case name == peersName && !peers:
			notes = append(notes, fmt.Sprintf("%s off (%s)", name, peersOption))
		case !p.Installed:
			status = "warn"
			notes = append(notes, name+" plugin not installed; run tmag setup (prefix + M-a)")
		case !p.Enabled:
			status = "warn"
			notes = append(notes, name+" plugin disabled; run tmag setup to enable it")
		case p.outdated():
			status = "warn"
			notes = append(notes, fmt.Sprintf("%s plugin %s, binary %s; run tmag setup to update", name, p.Version, version.Version))
		default:
			notes = append(notes, name+" plugin "+p.Version+" enabled")
		}
	}
	return status, strings.Join(notes, "; ")
}

func claudeCheck(h host, peers bool) check {
	if h.ProbeFailure != "" {
		return check{"claude", "warn", "host probe incomplete: " + h.ProbeFailure + "; installation status unknown"}
	}
	if !h.Present {
		return check{"claude", "skip", "Claude Code not installed"}
	}
	status, note := pluginNote(h, peers)
	return check{"claude", status, h.Version + "; " + note}
}

func codexCheck(ctx context.Context, h host, peers bool) (check, bool) {
	if ctx.Err() != nil {
		return check{"codex", "warn", "host/daemon probe incomplete within shared budget; installation status unknown"}, false
	}
	if h.ProbeFailure != "" {
		return check{"codex", "warn", "host probe incomplete: " + h.ProbeFailure + "; installation status unknown"}, false
	}
	if !h.Present {
		return check{"codex", "skip", "Codex not installed"}, true
	}
	complete := !h.ApprovalUnknown
	status, note := pluginNote(h, peers)
	notes := []string{h.Version, note}
	switch {
	case status != "ok":
	case h.ApprovalUnknown:
		status = "warn"
		notes = append(notes, "hook approval evidence unavailable")
	case !h.HookApproval:
		status = "warn"
		notes = append(notes, "plugin hooks not approved yet; approve them in Codex with /hooks")
	default:
		// Codex keeps an approval only for the hooks it was given for, and
		// does not say whether the current ones match.
		notes = append(notes, "hook approval found, not verifiable for the current hooks; if states stop updating, approve again with /hooks")
	}
	if pid, ok, err := codexDaemonContext(ctx); err != nil {
		complete = false
		status = "warn"
		notes = append(notes, "daemon discovery unavailable within shared budget")
	} else if ok {
		notes = append(notes, "threads run in the shared daemon: panes show them through thread-id in the terminal title")
		if problem, err := daemonBinaryProblemContext(ctx, pid); err != nil {
			complete = false
			status = "warn"
			notes = append(notes, "daemon hook binary lookup unavailable within shared budget")
		} else if problem != "" {
			status = "warn"
			notes = append(notes, problem)
		}
	}
	return check{"codex", status, strings.Join(notes, "; ")}, complete && ctx.Err() == nil
}

// daemonBinaryProblem checks that the Codex daemon, which keeps the environment
// it started with, can still find tmag for the hooks it runs.
func daemonBinaryProblem(pid int) string {
	problem, _ := daemonBinaryProblemContext(context.Background(), pid)
	return problem
}
func daemonBinaryProblemContext(ctx context.Context, pid int) (string, error) {
	env, err := proc.EnvironContext(ctx, pid, "PATH", "HOME", "XDG_CONFIG_HOME", "TMUX_PLUGIN_MANAGER_PATH")
	if err != nil {
		return "", err
	}
	found, err := hookBinariesContext(ctx, func(k string) string { return env[k] })
	if err != nil {
		return "", err
	}
	const restart = "restart it from a shell inside tmux: codex app-server daemon restart (this interrupts running Codex work)"
	switch {
	case len(found) == 0:
		return "the daemon cannot find tmag, so its hooks cannot run; " + restart, nil
	case !inPluginDir(found[0]) && inMiseInstalls(found[0]):
		if _, shim := viaMiseShim(found); !shim {
			return "the daemon reaches tmag through mise's versioned install directory and loses it after an upgrade; " + restart, nil
		}
	}
	return "", ctx.Err()
}

func doctorPeersWanted(ctx context.Context) (bool, error) {
	servers, err := tmux.ServersLimitContext(ctx, policy.MaxServers)
	if err != nil {
		return false, err
	}
	wanted := true
	for _, srv := range servers {
		value, err := srv.GlobalOptionContext(ctx, peersOption)
		if err != nil {
			if absent, probeErr := srv.Absent(ctx); probeErr == nil && absent {
				continue
			}
			return false, err
		}
		if value == "off" {
			wanted = false
		}
	}
	return wanted, ctx.Err()
}
