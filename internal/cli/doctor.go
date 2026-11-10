package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
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
	ctx, stopProbe := context.WithTimeout(ctx, 2*time.Second)
	defer stopProbe()
	checks := []check{a.checkRecords(ctx), a.checkState(ctx), a.checkHookErrors(ctx), checkTmux(ctx), checkPlugin(ctx), checkBinary(), a.checkNotify(ctx), checkServe(ctx)}
	peers, err := doctorPeers(ctx)
	if err != nil {
		checks = append(checks, check{"claude", "warn", "host plugin checks incomplete: tmux policy unavailable"}, check{"codex", "warn", "host plugin checks incomplete: tmux policy unavailable"})
	} else {
		checks = append(checks, claudeCheck(claudeHostContext(ctx), peers), codexCheck(ctx, codexHostContext(ctx), peers))
	}

	failed := false
	for _, c := range checks {
		failed = failed || c.Status == "fail"
	}
	if a.json {
		a.emit(map[string]any{"version": version.Version, "checks": checks})
	} else {
		marks := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "-"}
		for _, c := range checks {
			fmt.Fprintf(a.stdout, "%s %-8s %s\n", marks[c.Status], c.Name, c.Detail)
		}
	}
	if failed {
		return ExitError
	}
	return ExitOK
}

func checkTmux(ctx context.Context) check {
	out, err := run(ctx, "tmux", "-V")
	if err != nil || hostMetadata(out, 128) == "unknown" {
		return check{"tmux", "warn", "tmux version probe unavailable"}
	}
	v := strings.TrimPrefix(out, "tmux ")
	if !tmux.VersionAtLeast(v, minTmuxMajor, minTmuxMinor) {
		return check{"tmux", "fail", fmt.Sprintf("%s; tmag needs %d.%d or newer", v, minTmuxMajor, minTmuxMinor)}
	}
	live := 0
	servers, err := doctorServers(ctx)
	if err != nil {
		return check{"tmux", "warn", "server discovery incomplete"}
	}
	for _, s := range servers {
		if _, err := s.Version(ctx); err == nil {
			live++
		} else {
			return check{"tmux", "warn", "server version probe incomplete"}
		}
	}
	return check{"tmux", "ok", fmt.Sprintf("%s, %d server(s) running", v, live)}
}

// checkPlugin reports whether the tmux plugin is loaded, and at which version.
func checkPlugin(ctx context.Context) check {
	loaded, stale := 0, ""
	servers, err := doctorServers(ctx)
	if err != nil {
		return check{"plugin", "warn", "server discovery incomplete"}
	}
	for _, s := range servers {
		value, err := s.GlobalOptionContext(ctx, "@tmag-plugin-version")
		if err != nil {
			return check{"plugin", "warn", "plugin option unavailable"}
		}
		switch v := value; v {
		case "":
		case version.Version:
			loaded++
		default:
			stale = v
		}
	}
	switch {
	case stale != "":
		return check{"plugin", "warn", "loaded plugin version differs; reload tmux.conf or update with prefix + U"}
	case loaded == 0:
		return check{"plugin", "warn", "tmux plugin not loaded; add set -g @plugin '" + repo + "' to tmux.conf and press prefix + I"}
	}
	return check{"plugin", "ok", fmt.Sprintf("tmux plugin %s loaded on %d server(s)", version.Version, loaded)}
}

func checkBinary() check {
	self, _ := os.Executable()
	return binaryCheck(self, hookBinaries(os.Getenv))
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
	dir := a.log.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return check{"state", "fail", fmt.Sprintf("%s: %v", dir, err)}
	}
	seq, err := a.log.Head(ctx)
	if err != nil {
		return check{"state", "fail", fmt.Sprintf("%s: %v", dir, err)}
	}
	if seq == 0 {
		return check{"state", "warn", fmt.Sprintf("%s: no events yet; are the agent plugins installed and trusted?", dir)}
	}
	events, _, err := a.log.Since(ctx, seq-1)
	if err != nil {
		return check{"state", "fail", fmt.Sprintf("%s: event log unreadable: %v", dir, err)}
	}
	if len(events) == 0 {
		return check{"state", "warn", "last event unavailable"}
	}
	last := events[len(events)-1]
	return check{"state", "ok", fmt.Sprintf("%s, last event seq %d at %s", dir, last.Seq, last.Time.UTC().Format(time.RFC3339))}
}

func (a *app) checkHookErrors(ctx context.Context) check {
	data, err := contextio.ReadFile(ctx, filepath.Join(a.log.Dir, "hook.log"), 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return check{"hooks", "ok", "no hook failures recorded"}
	}
	if err != nil {
		return check{"hooks", "warn", "hook log unreadable or probe incomplete"}
	}
	count, latest := 0, ""
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, line := range strings.Split(string(data), "\n") {
		if ctx.Err() != nil {
			return check{"hooks", "warn", "hook log probe incomplete"}
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[0])
		if err != nil || !at.After(cutoff) {
			continue
		}
		count++
		latest = "legacy record (message redacted)"
		if len(fields) == 3 && (fields[1] == "claude" || fields[1] == "codex" || fields[1] == "notify" || fields[1] == "hook") {
			switch fields[2] {
			case "failed", "timeout", "canceled", "records_unavailable", "limited":
				latest = fields[1] + " " + fields[2]
			}
		}
	}
	return check{"hooks", "ok", fmt.Sprintf("%d historical failure(s) in 24h; latest: %s; current health checked separately", count, latest)}
}

// checkRecords reports the session records, and those tmag cannot read,
// which views leave out until their sessions' next hooks write them anew.
func (a *app) checkRecords(ctx context.Context) check {
	recs, faults, err := a.store().All(ctx)
	if err != nil {
		return check{"records", "warn", "record probe incomplete"}
	}
	var latest time.Time
	for _, r := range recs {
		if r.LastHookAt.After(latest) {
			latest = r.LastHookAt
		}
	}
	contact := "unknown"
	if !latest.IsZero() {
		contact = latest.UTC().Format(time.RFC3339Nano)
	}
	if len(faults) > 0 {
		return check{"records", "warn", fmt.Sprintf("%d readable and %d unreadable record(s), first %s; views leave them out until their sessions' next hooks rewrite them", len(recs), len(faults), faults[0])}
	}
	return check{"records", "ok", fmt.Sprintf("%d record(s); latest hook contact %s (not state freshness)", len(recs), contact)}
}

var errDoctorSample = errors.New("doctor sample complete")

func doctorServers(ctx context.Context) ([]tmux.Server, error) {
	servers, err := tmux.ServersContext(ctx)
	if err == nil && len(servers) > 16 {
		err = errDoctorSample
	}
	return servers, err
}
func doctorPeers(ctx context.Context) (bool, error) {
	servers, err := doctorServers(ctx)
	if err != nil {
		return false, err
	}
	peers := true
	for _, s := range servers {
		v, err := s.GlobalOptionContext(ctx, peersOption)
		if err != nil {
			return false, err
		}
		if v == "off" {
			peers = false
		}
	}
	return peers, ctx.Err()
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
	if h.Unavailable {
		return check{"claude", "warn", "host probe incomplete; installation status unknown"}
	}
	if !h.Present {
		return check{"claude", "skip", "Claude Code not installed"}
	}
	status, note := pluginNote(h, peers)
	return check{"claude", status, h.Version + "; " + note}
}

func codexCheck(ctx context.Context, h host, peers bool) check {
	if h.Unavailable || ctx.Err() != nil {
		return check{"codex", "warn", "host probe incomplete; installation status unknown"}
	}
	if !h.Present {
		return check{"codex", "skip", "Codex not installed"}
	}
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
	if _, ok, err := codexDaemonContext(ctx); err != nil {
		status = "warn"
		notes = append(notes, "daemon probe incomplete")
	} else if ok {
		notes = append(notes, "threads run in the shared daemon; panes require thread-id; daemon environment not checked")
	}
	return check{"codex", status, strings.Join(notes, "; ")}
}

// daemonBinaryProblem checks that the Codex daemon, which keeps the environment
// it started with, can still find tmag for the hooks it runs.
func daemonBinaryProblem(pid int) string {
	env, readable := proc.Environ(pid, "PATH", "HOME", "XDG_CONFIG_HOME", "TMUX_PLUGIN_MANAGER_PATH")
	if !readable {
		return ""
	}
	found := hookBinaries(func(k string) string { return env[k] })
	const restart = "restart it from a shell inside tmux: codex app-server daemon restart (this interrupts running Codex work)"
	switch {
	case len(found) == 0:
		return "the daemon cannot find tmag, so its hooks cannot run; " + restart
	case !inPluginDir(found[0]) && inMiseInstalls(found[0]):
		if _, shim := viaMiseShim(found); !shim {
			return "the daemon reaches tmag through mise's versioned install directory and loses it after an upgrade; " + restart
		}
	}
	return ""
}
