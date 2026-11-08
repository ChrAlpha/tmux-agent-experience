package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
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
	checks := []check{checkTmux(), checkPlugin(), checkBinary(), a.checkState(ctx), a.checkHookErrors(), a.checkNotify(), checkServe()}
	peers := peersWanted(context.Background())
	checks = append(checks, claudeCheck(claudeHost(), peers), codexCheck(codexHost(), peers), a.checkAgents())

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

func checkTmux() check {
	out, err := run("tmux", "-V")
	if err != nil {
		return check{"tmux", "fail", "tmux not found"}
	}
	v := strings.TrimPrefix(out, "tmux ")
	if !tmux.VersionAtLeast(v, minTmuxMajor, minTmuxMinor) {
		return check{"tmux", "fail", fmt.Sprintf("%s; tmag needs %d.%d or newer", v, minTmuxMajor, minTmuxMinor)}
	}
	live := 0
	for _, s := range tmux.Servers() {
		if _, err := s.Version(context.Background()); err == nil {
			live++
		}
	}
	return check{"tmux", "ok", fmt.Sprintf("%s, %d server(s) running", v, live)}
}

// checkPlugin reports whether the tmux plugin is loaded, and at which version.
func checkPlugin() check {
	loaded, stale := 0, ""
	for _, s := range tmux.Servers() {
		switch v := s.GlobalOption(context.Background(), "@tmag-plugin-version"); v {
		case "":
		case version.Version:
			loaded++
		default:
			stale = v
		}
	}
	switch {
	case stale != "":
		return check{"plugin", "warn", fmt.Sprintf("tmux plugin %s loaded, binary is %s; reload tmux.conf or update with prefix + U", stale, version.Version)}
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
	cursor, err := a.log.Head(ctx)
	if err != nil {
		return check{"state", "fail", fmt.Sprintf("%s: %v", dir, err)}
	}
	if cursor.Seq == 0 {
		return check{"state", "warn", fmt.Sprintf("%s: no events yet; are the agent plugins installed and trusted?", dir)}
	}
	events, _, err := a.log.Since(ctx, cursor.Seq-1)
	if err != nil || len(events) == 0 {
		return check{"state", "ok", dir}
	}
	last := events[len(events)-1]
	return check{"state", "ok", fmt.Sprintf("%s, last event %s from %s %s ago", dir, last.Event, last.Agent, time.Since(last.Time).Round(time.Second))}
}

func (a *app) checkHookErrors() check {
	f, err := os.Open(filepath.Join(a.log.Dir, "hook.log"))
	if errors.Is(err, os.ErrNotExist) {
		return check{"hooks", "ok", "no hook errors recorded"}
	}
	if err != nil {
		return check{"hooks", "warn", err.Error()}
	}
	defer f.Close()
	var recent []string
	cutoff := time.Now().Add(-24 * time.Hour)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		ts, _, _ := strings.Cut(line, " ")
		if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(cutoff) {
			recent = append(recent, line)
		}
	}
	if len(recent) == 0 {
		return check{"hooks", "ok", "no hook errors in the last 24h"}
	}
	return check{"hooks", "warn", fmt.Sprintf("%d hook error(s) in the last 24h; latest: %s", len(recent), recent[len(recent)-1])}
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
	if !h.Present {
		return check{"claude", "skip", "Claude Code not installed"}
	}
	status, note := pluginNote(h, peers)
	return check{"claude", status, h.Version + "; " + note}
}

func codexCheck(h host, peers bool) check {
	if !h.Present {
		return check{"codex", "skip", "Codex not installed"}
	}
	status, note := pluginNote(h, peers)
	notes := []string{h.Version, note}
	switch {
	case status != "ok":
	case !h.HookApproval:
		status = "warn"
		notes = append(notes, "plugin hooks not approved yet; approve them in Codex with /hooks")
	default:
		// Codex keeps an approval only for the hooks it was given for, and
		// does not say whether the current ones match.
		notes = append(notes, "hook approval found, not verifiable for the current hooks; if states stop updating, approve again with /hooks")
	}
	if pid, ok := codexDaemon(); ok {
		notes = append(notes, "threads run in the shared daemon: panes show them through thread-id in the terminal title")
		if problem := daemonBinaryProblem(pid); problem != "" {
			status = "warn"
			notes = append(notes, problem)
		}
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

func (a *app) checkAgents() check {
	v, _, err := query.Load(context.Background(), a.store())
	if err != nil {
		return check{"agents", "warn", err.Error()}
	}
	if len(v.Panes) == 0 && len(v.Sessions) == 0 {
		return check{"agents", "ok", "no agents running"}
	}
	shown := 0
	for _, p := range v.Panes {
		if p.Session != "" {
			shown++
		}
	}
	note := fmt.Sprintf("%d session(s); %d of %d agent pane(s) show one", len(v.Sessions), shown, len(v.Panes))
	if hint := codexHint(v); hint != "" {
		return check{"agents", "warn", note + ". " + hint}
	}
	if shown < len(v.Panes) {
		return check{"agents", "ok", note + " (the others have not reported yet)"}
	}
	return check{"agents", "ok", note}
}
