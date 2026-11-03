// Package proc snapshots the process table and classifies coding-agent processes.
package proc

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Process is one row of the process table.
type Process struct {
	PID  int
	PPID int
	// PGID is the process group; TPGID the foreground process group of the
	// process's controlling terminal, or <= 0 when it has none.
	PGID  int
	TPGID int
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
}

// NewTable indexes the given processes.
func NewTable(ps []Process) *Table {
	t := &Table{byPID: make(map[int]Process, len(ps)), children: make(map[int][]int, len(ps))}
	for _, p := range ps {
		t.byPID[p.PID] = p
		t.children[p.PPID] = append(t.children[p.PPID], p.PID)
	}
	return t
}

// Snapshot reads the current process table from /proc when available, otherwise from ps.
func Snapshot() (*Table, error) {
	if ps, err := readProcFS(); err == nil {
		return NewTable(ps), nil
	}
	ps, err := readPS()
	if err != nil {
		return nil, err
	}
	return NewTable(ps), nil
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
)

// AgentKind classifies an interactive coding-agent process, or returns "".
func AgentKind(p Process) string {
	switch {
	case p.Comm == "claude":
		return Claude
	case strings.HasPrefix(p.Comm, "codex"):
		if IsCodexService(p) {
			return ""
		}
		return Codex
	case p.Comm == "node" || p.Comm == "bun":
		switch {
		case strings.Contains(p.Args, "claude-code") || claudeScriptRE.MatchString(p.Args):
			return Claude
		case strings.Contains(p.Args, codexPackage):
			if IsCodexService(p) {
				return ""
			}
			return Codex
		}
	}
	return ""
}

// claudeScriptRE matches the claude launcher script run by node, as in
// "node /opt/homebrew/bin/claude" or "node /usr/local/bin/claude --resume".
var claudeScriptRE = regexp.MustCompile(`/claude(\s|$)`)

// codexPackage marks Codex run through its npm launcher.
const codexPackage = "@openai/codex"

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

func readProcFS() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return nil, err
	}
	ps := make([]Process, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, ok := readProcEntry(pid)
		if ok {
			ps = append(ps, p)
		}
	}
	return ps, nil
}

func readProcEntry(pid int) (Process, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	stat, err := os.ReadFile(dir + "/stat")
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
	if cmdline, err := os.ReadFile(dir + "/cmdline"); err == nil {
		p.Argv = strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		p.Args = strings.TrimSpace(strings.Join(p.Argv, " "))
	}
	return p, true
}

func readPS() ([]Process, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "tpgid=", "-o", "args=").Output()
	if err != nil {
		return nil, err
	}
	var ps []Process
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		var n [4]int
		var bad bool
		for i := range n {
			v, err := strconv.Atoi(f[i])
			n[i], bad = v, bad || err != nil
		}
		if bad {
			continue
		}
		ps = append(ps, Process{
			PID: n[0], PPID: n[1], PGID: n[2], TPGID: n[3],
			Comm: filepath.Base(f[4]), Args: strings.Join(f[4:], " "),
		})
	}
	return ps, sc.Err()
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
