// Package proc snapshots the process table and classifies coding-agent processes.
package proc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
)

// Process is one row of the process table.
type Process struct {
	// Uncertain preserves an unreadable PID without authorizing attachment or control.
	Uncertain bool
	PID       int
	PPID      int
	// PGID is the process group; TPGID the foreground process group of the
	// process's controlling terminal, or <= 0 when it has none.
	PGID  int
	TPGID int
	// Start identifies the process among every one that held its pid: its
	// start time as the platform reports it, compared only for equality.
	Start string
	Comm  string // executable base name
	Args  string // full command line, space separated
	// Argv is the command line split into its arguments, when the platform
	// keeps their boundaries (Linux); otherwise Args split on spaces.
	Argv []string
}

// Foreground reports whether p's process group holds its terminal, so that
// input typed there reaches p.
func (p Process) Foreground() bool { return p.PGID > 0 && p.PGID == p.TPGID }

// Table is an immutable process snapshot indexed by pid and parent.
type Table struct {
	byPID    map[int]Process
	children map[int][]int
	complete bool
}

// NewTable indexes the given processes.
func NewTable(ps []Process) *Table {
	t := &Table{byPID: make(map[int]Process, len(ps)), children: make(map[int][]int, len(ps)), complete: true}
	for _, p := range ps {
		if p.Uncertain {
			t.complete = false
		}
		t.byPID[p.PID] = p
		t.children[p.PPID] = append(t.children[p.PPID], p.PID)
	}
	return t
}

// Complete reports whether every enumerated PID could be inspected. Missing
// facts in an incomplete table cannot authorize destructive reconciliation.
func (t *Table) Complete() bool { return t.complete }

// Snapshot reads the current process table from /proc when available, otherwise from ps.
func Snapshot() (*Table, error) { return SnapshotContext(context.Background()) }

// SnapshotContext never returns a canceled or partially enumerated table.
// Unreadable individual PIDs remain uncertain: absence is not death evidence.
func SnapshotContext(ctx context.Context) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ps, err := readProcFSContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ps, err = readPSContext(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	table := NewTable(ps)
	if !table.Complete() {
		return nil, fmt.Errorf("process table incomplete")
	}
	return table, nil
}

// Get returns the process with the given pid.
func (t *Table) Get(pid int) (Process, bool) {
	p, ok := t.byPID[pid]
	return p, ok
}

// All returns every process in the snapshot, in no particular order.
func (t *Table) All() []Process {
	out := make([]Process, 0, len(t.byPID))
	for _, p := range t.byPID {
		out = append(out, p)
	}
	return out
}

// Ancestors returns pid followed by its ancestors, nearest first.
func (t *Table) Ancestors(pid int) []int {
	var out []int
	seen := map[int]bool{}
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		out = append(out, pid)
		p, ok := t.byPID[pid]
		if !ok {
			break
		}
		pid = p.PPID
	}
	return out
}

// Descendants returns root and all of its descendants in breadth-first order.
func (t *Table) Descendants(root int) []Process {
	var out []Process
	queue := []int{root}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := t.byPID[pid]; ok {
			out = append(out, p)
		}
		queue = append(queue, t.children[pid]...)
	}
	return out
}

// Agent kinds recognised by tmag.
const (
	Claude = "claude"
	Codex  = "codex"
	Pi     = "pi"
	OMP    = "omp" // oh-my-pi
)

// Agents lists every agent kind, in the order tmag reports them.
var Agents = []string{Claude, Codex, Pi, OMP}

// IsAgent reports whether kind names an agent tmag supports.
func IsAgent(kind string) bool { return slices.Contains(Agents, kind) }

// AgentKind classifies an interactive coding-agent process, a client the user
// works in, or returns "". Background services of an agent are not clients;
// see ServiceKind.
func AgentKind(p Process) string {
	if p.Uncertain {
		return ""
	}
	kind := programKind(p)
	if kind == "" || serves(kind, p) || isOMPWorker(p) {
		return ""
	}
	return kind
}

// programKind names the agent whose program p runs, as a client or a
// service, or returns "".
func programKind(p Process) string {
	switch {
	case isClaude(p.Comm):
		return Claude
	case strings.HasPrefix(p.Comm, "codex"):
		return Codex
	case p.Comm == "pi" || p.Comm == piRPC:
		return Pi
	case p.Comm == "omp" || filepath.Base(argv0(p)) == "omp":
		// oh-my-pi names its process omp on Linux; elsewhere ps shows the
		// command it was started as.
		return OMP
	case p.Comm == "node" || p.Comm == "bun":
		switch {
		case strings.Contains(p.Args, "claude-code") || claudeScriptRE.MatchString(p.Args):
			return Claude
		case strings.Contains(p.Args, codexPackage):
			return Codex
		case strings.Contains(p.Args, ompScript) || ompScriptRE.MatchString(p.Args):
			return OMP
		case strings.Contains(p.Args, piScript):
			return Pi
		}
	}
	return ""
}

// serves reports whether p, a program of agent kind, is one of its services
// rather than a client.
func serves(kind string, p Process) bool {
	switch kind {
	case Claude:
		return IsClaudeService(p)
	case Codex:
		return IsCodexService(p)
	case Pi:
		return p.Comm == piRPC
	case OMP:
		return isOMPService(p)
	}
	return false
}

// ServiceKind names the agent whose background service p is, or returns "":
// a shared Codex app-server, Claude's supervisor and the per-session hosts it
// runs background sessions in, or Pi or oh-my-pi serving another program
// over their standard input instead of a person at a terminal.
func ServiceKind(p Process) string {
	if p.Uncertain {
		return ""
	}
	switch {
	case IsClaudeService(p):
		return Claude
	case IsCodexService(p):
		return Codex
	}
	if kind := programKind(p); (kind == Pi || kind == OMP) && serves(kind, p) {
		return kind
	}
	return ""
}

// argv0 is the command p was started as.
func argv0(p Process) string {
	if len(p.Argv) > 0 {
		return p.Argv[0]
	}
	first, _, _ := strings.Cut(p.Args, " ")
	return first
}

// claudeServices are the Claude Code subcommands of its background
// supervisor: the supervisor itself, the pty host of each background session
// and the spare it keeps ready.
var claudeServices = map[string]bool{"daemon": true, "bg-pty-host": true, "bg-spare": true}

// IsClaudeService reports whether p is part of Claude Code's background
// supervisor rather than an interactive client. Background sessions run as a
// worker under a pty host; a worker looks like an interactive client, and
// HostOf tells them apart by the worker's parent.
//
// A pty host or spare titles itself as one argv string, "claude bg-pty-host"
// or "claude bg-spare", followed by the flag of the same role. The daemon and
// a plain executable name the role in the argument after the executable; a
// node-run package names it after the claude-code script.
func IsClaudeService(p Process) bool {
	argv := p.Argv
	if argv == nil {
		argv = strings.Fields(p.Args)
	}
	if len(argv) == 0 {
		return false
	}
	if role, ok := strings.CutPrefix(argv[0], "claude "); ok && claudeServices[role] {
		return true
	}
	if isClaude(filepath.Base(argv[0])) || strings.Contains(argv[0], "claude-code") {
		return len(argv) > 1 && claudeServices[argv[1]]
	}
	for i, arg := range argv[1:] {
		if strings.Contains(arg, "claude-code") {
			return i+2 < len(argv) && claudeServices[argv[i+2]]
		}
	}
	return false
}

// isClaude reports whether name is Claude Code's native executable: claude,
// or claude.exe, as its npm package installs it on every platform.
func isClaude(name string) bool { return name == "claude" || name == "claude.exe" }

// Role is how a hook's host process runs a session.
type Role string

const (
	// Terminal: the session runs in an interactive client in a terminal.
	Terminal Role = "terminal"
	// Service: the session runs in a background service that no terminal owns.
	Service Role = "service"
)

// Host is the process a hook's session runs in: the nearest interactive
// agent or agent service above the hook.
type Host struct {
	Process Process
	Kind    string
	Role    Role
}

// HostOf walks up from pid to the nearest agent client or agent service. A
// Claude Code background worker looks like a client, but its parent is one of
// its services: the walk goes on to that service, so the session stays
// service-hosted.
func (t *Table) HostOf(pid int) (Host, bool) {
	for _, a := range t.Ancestors(pid) {
		p, ok := t.Get(a)
		if !ok {
			continue
		}
		if kind := ServiceKind(p); kind != "" {
			return Host{Process: p, Kind: kind, Role: Service}, true
		}
		if kind := AgentKind(p); kind != "" {
			if kind == Claude && t.claudeWorker(p) {
				continue
			}
			return Host{Process: p, Kind: kind, Role: Terminal}, true
		}
	}
	return Host{}, false
}

// claudeWorker reports whether p is a background session's Claude Code
// worker: a client whose parent is a Claude service. Its own argv cannot tell
// it from an interactive client; only its parent can.
func (t *Table) claudeWorker(p Process) bool {
	parent, ok := t.Get(p.PPID)
	return ok && ServiceKind(parent) == Claude
}

// Running reports whether the process with this pid still runs and, when
// start is set, is the one that started then. It reads one process where
// the platform allows (Linux) and the whole table otherwise.
func Running(pid int, start string) bool { return RunningContext(context.Background(), pid, start) }

// RunningContext conservatively preserves liveness on an unreadable process table.
// It is not proof of a control channel.
func RunningContext(ctx context.Context, pid int, start string) bool {
	if ctx.Err() != nil {
		return true
	}
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		p, ok := readProcEntryContext(ctx, pid)
		if ctx.Err() != nil {
			return true
		}
		if !ok {
			_, err := os.Stat("/proc/" + strconv.Itoa(pid))
			return !os.IsNotExist(err)
		}
		return start == "" || p.Start == start
	}
	t, err := SnapshotContext(ctx)
	return err != nil || t.Alive(pid, start)
}

// Alive reports whether the process with p's pid is still p.
func (t *Table) Alive(pid int, start string) bool {
	q, ok := t.Get(pid)
	return ok && (q.Uncertain || start == "" || q.Start == start)
}

// claudeScriptRE matches the claude launcher script run by node, as in
// "node /opt/homebrew/bin/claude" or "node /usr/local/bin/claude --resume".
var claudeScriptRE = regexp.MustCompile(`/claude(\s|$)`)

// codexPackage marks Codex run through its npm launcher.
const codexPackage = "@openai/codex"

// Pi 1.5 names its process pi, and piRPC when started from its RPC entry
// point, which programs embedding it run; before it does, node runs its
// package's script (piScript). Its CLI names the process pi in every mode:
// tmag's extension reports the mode instead.
const (
	piRPC    = "pi-rpc"
	piScript = "pi-coding-agent/dist/"
)

// ompScript marks oh-my-pi run by bun from its npm package, which shares
// Pi's package name under its own scope; ompScriptRE matches the omp
// launcher script bun runs, as in "bun /home/me/.bun/bin/omp".
const ompScript = "@oh-my-pi/pi-coding-agent/dist/"

var ompScriptRE = regexp.MustCompile(`/omp(\s|$)`)

// ompServiceModes are the modes of oh-my-pi 18.18 in which another program
// drives it over its standard input.
var ompServiceModes = map[string]bool{"rpc": true, "rpc-ui": true, "acp": true}

// arguments returns p's arguments after the command, split as the platform
// keeps them (on spaces without boundaries).
func arguments(p Process) []string {
	argv := p.Argv
	if argv == nil {
		argv = strings.Fields(p.Args)
	}
	if len(argv) == 0 {
		return nil
	}
	return argv[1:]
}

// isOMPWorker reports whether p is one of the helper processes oh-my-pi
// starts by running itself again with a worker argument (__omp_worker_*):
// they share its name and serve no session.
func isOMPWorker(p Process) bool {
	args := arguments(p)
	for _, a := range args[:min(len(args), 2)] {
		if strings.HasPrefix(a, "__omp_worker_") {
			return true
		}
	}
	return false
}

// isOMPService reports whether p is oh-my-pi run for another program: in an
// RPC or ACP mode, or as its acp command.
func isOMPService(p Process) bool {
	args := arguments(p)
	for i, a := range args {
		switch {
		case a == "--mode" && i+1 < len(args):
			return ompServiceModes[args[i+1]]
		case strings.HasPrefix(a, "--mode="):
			return ompServiceModes[strings.TrimPrefix(a, "--mode=")]
		case a == "acp" && i == 0:
			return true
		}
	}
	return false
}

// codexServices are the Codex subcommands that run a background service
// rather than an interactive client.
var codexServices = map[string]bool{"app-server": true, "exec-server": true, "code-mode-host": true}

// Options of the Codex 0.166 root command that take a separate value; any of
// them may come before the subcommand (codex -c k=v app-server).
var (
	codexValueLong = map[string]bool{
		"config": true, "enable": true, "disable": true, "remote": true, "remote-auth-token-env": true,
		"image": true, "model": true, "local-provider": true, "profile": true, "sandbox": true,
		"ask-for-approval": true, "cd": true, "add-dir": true,
	}
	codexValueShort = map[byte]bool{'c': true, 'i': true, 'm': true, 'p': true, 's': true, 'a': true, 'C': true}
)

// IsCodexService reports whether p is a Codex background service (app-server
// daemon, exec server, code-mode host) rather than an interactive client. The
// service is named by the subcommand: the first argument after the codex
// executable or launcher script that is neither an option nor an option's
// value. A prompt that merely mentions one is not a service.
func IsCodexService(p Process) bool {
	if strings.HasPrefix(p.Comm, "codex-code-mode") {
		return true
	}
	argv, split := p.Argv, false
	if argv == nil {
		// ps joins arguments with spaces, so a prompt may look like several.
		argv, split = strings.Fields(p.Args), true
	}
	for i, arg := range argv {
		if !strings.HasPrefix(filepath.Base(arg), "codex") && !strings.Contains(arg, codexPackage) {
			continue
		}
		cmd, rest := codexCommand(argv[i+1:])
		if !codexServices[cmd] {
			return false
		}
		// Without argument boundaries, accept only what a service is started
		// with: nothing more, flags, or the daemon's own subcommand.
		return !split || len(rest) == 0 || strings.HasPrefix(rest[0], "-") || rest[0] == "daemon"
	}
	return false
}

// codexCommand returns the first positional argument of a Codex command line
// (a subcommand or the prompt) and the arguments after it.
func codexCommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return args[i+1], args[i+2:]
			}
			return "", nil
		case strings.HasPrefix(a, "--"):
			if codexValueLong[a[2:]] { // not --name=value
				i++
			}
		case len(a) == 2 && a[0] == '-':
			if codexValueShort[a[1]] { // not -Xvalue
				i++
			}
		case strings.HasPrefix(a, "-") && a != "-":
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// FindAgent returns the nearest agent process at or below root.
func (t *Table) FindAgent(root int) (Process, string, bool) {
	for _, p := range t.Descendants(root) {
		if kind := AgentKind(p); kind != "" {
			return p, kind, true
		}
	}
	return Process{}, "", false
}

func readProcFSContext(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return nil, err
	}
	var ps []Process
	err := contextio.Entries(ctx, "/proc", func(e os.DirEntry) error {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			return nil //nolint:nilerr // Non-PID names such as self are expected /proc entries, not an incomplete process view.
		}
		p, ok := readProcEntryContext(ctx, pid)
		if !ok {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(err) {
				return nil
			}
			p = Process{PID: pid, Uncertain: true}
		}
		ps = append(ps, p)
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return ps, nil
}

func readProcEntryContext(ctx context.Context, pid int) (Process, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	stat, err := contextio.ReadFile(ctx, dir+"/stat", 64<<10)
	if err != nil {
		return Process{}, false
	}
	// Format: pid (comm) state ppid ...; comm may contain spaces and parentheses.
	open := bytes.IndexByte(stat, '(')
	end := bytes.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return Process{}, false
	}
	// After comm: state ppid pgrp session tty_nr tpgid ...
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 6 {
		return Process{}, false
	}
	ppid, err1 := strconv.Atoi(fields[1])
	pgid, err2 := strconv.Atoi(fields[2])
	tpgid, err3 := strconv.Atoi(fields[5])
	if err1 != nil || err2 != nil || err3 != nil {
		return Process{}, false
	}
	p := Process{PID: pid, PPID: ppid, PGID: pgid, TPGID: tpgid, Comm: string(stat[open+1 : end])}
	if len(fields) > 19 {
		p.Start = fields[19] // starttime, in clock ticks after boot
	}
	if cmdline, err := contextio.ReadFile(ctx, dir+"/cmdline", 4<<20); err == nil {
		p.Argv = strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		p.Args = strings.TrimSpace(strings.Join(p.Argv, " "))
	}
	return p, true
}

func readPS() ([]Process, error) { return readPSContext(context.Background()) }

func readPSContext(parent context.Context) ([]Process, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	// lstart prints five words ("Tue Nov  3 09:12:22 2026"), before args so
	// that the command line keeps its spaces.
	cmd, cleanup := helper.Command(ctx, "ps", "-A", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "tpgid=", "-o", "lstart=", "-o", "args=")
	defer cleanup()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	const lstartWords = 5
	var ps []Process
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) < 4+lstartWords {
			return nil, fmt.Errorf("incomplete ps row")
		}
		var n [4]int
		var bad bool
		for i := range n {
			v, err := strconv.Atoi(f[i])
			n[i], bad = v, bad || err != nil
		}
		if bad {
			return nil, fmt.Errorf("invalid ps process identity")
		}
		args := f[4+lstartWords:]
		comm := ""
		if len(args) > 0 {
			comm = filepath.Base(args[0])
		}
		ps = append(ps, Process{
			PID: n[0], PPID: n[1], PGID: n[2], TPGID: n[3],
			Start: strings.Join(f[4:4+lstartWords], " "),
			Comm:  comm, Args: strings.Join(args, " "),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, fmt.Errorf("empty ps process table")
	}
	return ps, nil
}

// Environ returns the named variables a running process was started with,
// where the platform exposes them (Linux). Nothing else is read out; ok is
// false when the environment is unreadable.
func Environ(pid int, names ...string) (vars map[string]string, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, false
	}
	vars = map[string]string{}
	for _, kv := range bytes.Split(data, []byte{0}) {
		for _, name := range names {
			if v, found := bytes.CutPrefix(kv, []byte(name+"=")); found {
				vars[name] = string(v)
			}
		}
	}
	return vars, true
}

// LookPathAll returns every executable with the given name on a PATH value,
// in PATH order.
func LookPathAll(path, name string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			out = append(out, candidate)
		}
	}
	return out
}
