package proc

import (
	"path/filepath"
	"regexp"
	"strings"
)

// A declared agent is a program tmag does not know that reports itself:
// its hooks run `tmag hook PROGRAM`, naming the program it runs. The name
// proves only what any process name proves, so tmag takes it from the
// process table, never from the hook alone (see DeclaredHost); what tmag
// knows of the agent beyond that is what its hooks report.

// declaredName is what a declared agent may be named: a program's name, as
// a session's directory and tmux's formats keep it literally.
var declaredName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// notAgents are programs a person works in, or that run another program,
// rather than agents: shells, the programs that run one, and tmux and tmag.
// A hook naming one would make the shell a person types in an agent's. An
// interpreter names no agent either: its script does (see ProgramName).
var notAgents = map[string]bool{
	"sh": true, "ash": true, "dash": true, "bash": true, "rbash": true, "zsh": true, "ksh": true,
	"mksh": true, "pdksh": true, "oksh": true, "loksh": true, "yash": true, "posh": true, "tcsh": true,
	"csh": true, "fish": true, "nu": true, "xonsh": true, "elvish": true, "pwsh": true, "ion": true,
	"osh": true, "oil": true, "rc": true, "es": true, "busybox": true, "toybox": true,
	"login": true, "su": true, "sudo": true, "doas": true, "env": true, "nohup": true, "setsid": true,
	"timeout": true, "script": true, "screen": true, "ssh": true, "sshd": true, "mosh-server": true,
	"tmux": true, "tmag": true,
}

// Declarable reports whether name may name a declared agent: a program name
// that is none of the agents tmag knows, nor a shell, an interpreter, tmux or
// tmag.
func Declarable(name string) bool {
	return declaredName.MatchString(name) && !notAgents[name] && !interpreter(name) &&
		ProgramKind(Process{Comm: name, Argv: []string{name}}) == ""
}

// IsKind reports whether kind names an agent a session may belong to: one
// tmag knows, or a declared one.
func IsKind(kind string) bool { return IsAgent(kind) || Declarable(kind) }

// interpreter reports whether a process named comm runs the script its
// arguments name, which is then the program it runs.
func interpreter(comm string) bool { return runtimeName(comm) != "" }

// runtimeName is the interpreter an executable's name is, or "": node,
// which Node 26 names node-MainThread, bun, deno, or a python.
func runtimeName(name string) string {
	switch {
	case name == "node" || name == "node-MainThread":
		return "node"
	case name == "bun" || name == "deno":
		return name
	case strings.HasPrefix(name, "python"):
		return "python"
	}
	return ""
}

// commLen is how much of its executable's name Linux keeps as a process's
// name: a longer one is cut there, and the command line tells the rest.
const commLen = 15

// ProgramName is the name of the program p runs: its own name, as its
// executable or the program itself named the process (Linux keeps 15 bytes
// of it, and argv[0] the rest), or the script an interpreter runs when its
// first argument names it, as a script's #! line runs it (`node
// /usr/local/bin/gemini` runs gemini), with `python -m MODULE` the module's
// package and `bun run SCRIPT` and `deno run SCRIPT` their script. tmag
// guesses no other argument: an interpreter given options before its script
// runs itself as far as tmag tells. It is "" for a process the table cannot
// tell.
func ProgramName(p Process) string {
	if p.Uncertain {
		return ""
	}
	argv := p.Argv
	if argv == nil {
		argv = strings.Fields(p.Args)
	}
	name := p.Comm
	if len(argv) > 0 {
		// A process that names itself pads its command line with spaces.
		full := filepath.Base(strings.TrimRight(argv[0], " "))
		if len(name) == commLen && strings.HasPrefix(full, name) || runtimeName(full) != "" {
			name = full
		}
	}
	rt := runtimeName(name)
	if rt == "" || len(argv) < 2 {
		return name
	}
	first, script := argv[1], ""
	switch {
	case rt == "python" && first == "-m" && len(argv) > 2:
		pkg, _, _ := strings.Cut(argv[2], ".")
		return pkg
	case (rt == "bun" || rt == "deno") && first == "run" && len(argv) > 2:
		script = argv[2]
	case rt != "deno":
		script = first
	}
	if script == "" || strings.HasPrefix(script, "-") {
		return name
	}
	return filepath.Base(script)
}

// DeclaredHost is the process a declared agent's hook runs under, as HostOf
// is for agents tmag knows: the nearest agent above the hook, when it runs
// program. An agent tmag knows met first proves no declared host: the hook
// runs in that agent's tree, whatever it names.
func (t *Table) DeclaredHost(pid int, program string) (Host, bool) {
	if !Declarable(program) {
		return Host{}, false
	}
	for _, a := range t.Ancestors(pid) {
		p, ok := t.Get(a)
		if !ok {
			continue
		}
		if ServiceKind(p) != "" || AgentKind(p) != "" {
			return Host{}, false
		}
		if ProgramName(p) == program {
			return Host{Process: p, Kind: program, Role: Terminal}, true
		}
	}
	return Host{}, false
}
