package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDeclarable(t *testing.T) {
	for _, name := range []string{"gemini", "opencode", "cursor-agent", "kiro-cli", "qwen", "droid", "amp2", "a.b_c-d"} {
		if !Declarable(name) || !IsKind(name) {
			t.Errorf("%s may not name a declared agent", name)
		}
	}
	for _, name := range []string{
		// Agents tmag knows, and programs it takes for theirs.
		"claude", "claude.exe", "codex", "codex-code-mode", "pi", "pi-rpc", "omp",
		// Shells, interpreters, tmux and tmag.
		"sh", "ash", "bash", "zsh", "fish", "busybox", "sudo", "script", "node", "bun", "deno", "python3", "python3.12", "tmux", "tmag",
		// Not a program name tmag keeps literally.
		"", "Gemini", "-x", ".hidden", "a/b", "a b", "a:b", "thirty-three-characters-long-name",
	} {
		if Declarable(name) {
			t.Errorf("%q may name a declared agent", name)
		}
	}
	if !IsKind(Claude) || IsKind("bash") {
		t.Error("IsKind does not take the agents tmag knows and only declarable names")
	}
}

func TestProgramName(t *testing.T) {
	for _, c := range []struct {
		p    Process
		want string
	}{
		{Process{Comm: "gemini", Args: "gemini --yolo"}, "gemini"},
		// A script's #! line runs its interpreter with the script first, as
		// npm's launcher does; Node 26 names its process node-MainThread.
		{Process{Comm: "node", Argv: []string{"node", "/usr/local/bin/gemini", "--yolo"}}, "gemini"},
		{Process{Comm: "node-MainThread", Argv: []string{"/opt/node/bin/node", "/usr/local/bin/gemini"}}, "gemini"},
		// Earlier Node names its main thread MainThread; macOS's framework
		// Python runs as Python.
		{Process{Comm: "MainThread", Argv: []string{"/opt/hostedtoolcache/node/24.11.0/x64/bin/node", "/tmp/myagent"}}, "myagent"},
		{Process{Comm: "Python", Args: "/Library/Frameworks/Python.framework/Versions/3.13/Resources/Python.app/Contents/MacOS/Python /tmp/myagent"}, "myagent"},
		{Process{Comm: "python3.12", Argv: []string{"/usr/bin/python3.12", "/home/me/.local/bin/aider"}}, "aider"},
		{Process{Comm: "aider", Argv: []string{"/usr/bin/python3", "/home/me/.local/bin/aider"}}, "aider"},
		{Process{Comm: "python3", Argv: []string{"python3", "-m", "aider.main", "--yes"}}, "aider"},
		{Process{Comm: "bun", Args: "bun /home/me/.bun/bin/crush"}, "crush"},
		{Process{Comm: "bun", Args: "bun run agent.ts --flag"}, "agent.ts"},
		{Process{Comm: "deno", Args: "deno run main.ts"}, "main.ts"},
		// Options before the script leave it to guess: the interpreter runs
		// itself as far as tmag tells, whatever an option takes.
		{Process{Comm: "node-MainThread", Argv: []string{"node", "--redirect-warnings", "warnings.log", "agent.cjs"}}, "node"},
		{Process{Comm: "node", Argv: []string{"node", "--require", "./preload.js", "/srv/agent.mjs"}}, "node"},
		{Process{Comm: "node", Argv: []string{"node", "--no-warnings", "/usr/local/bin/gemini"}}, "node"},
		{Process{Comm: "node", Argv: []string{"node", "-e", "require('/usr/local/bin/gemini')"}}, "node"},
		{Process{Comm: "python3", Argv: []string{"python3", "-X", "utf8", "-m", "aider"}}, "python3"},
		{Process{Comm: "bun", Args: "bun run --preload ./p.ts agent.ts"}, "bun"},
		{Process{Comm: "deno", Args: "deno run -A main.ts"}, "deno"},
		{Process{Comm: "deno", Args: "deno main.ts"}, "deno"},
		// A program that names its own process, as Node's process.title
		// does, is that name.
		{Process{Comm: "myagent", Argv: []string{"myagent                    "}}, "myagent"},
		{Process{Comm: "claude-code-rou", Argv: []string{"claude-code-router            "}}, "claude-code-router"},
		// An interpreter that runs no script is itself.
		{Process{Comm: "node", Argv: []string{"node"}}, "node"},
		// Linux keeps 15 bytes of an executable's name; argv[0] has the rest.
		{Process{Comm: "claude-code-rou", Argv: []string{"/opt/bin/claude-code-router", "start"}}, "claude-code-router"},
		{Process{Comm: "claude-code-rou"}, "claude-code-rou"},
		{Process{Comm: "anything", Uncertain: true}, ""},
	} {
		if got := ProgramName(c.p); got != c.want {
			t.Errorf("ProgramName(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

// The interpreters as they run: what the kernel names their processes, and
// the command lines it keeps, are what ProgramName reads.
func TestProgramNameOfRealInterpreters(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no process table to read")
	}
	dir := t.TempDir()
	for _, c := range []struct {
		interpreter, body string
	}{
		{"node", "setTimeout(() => {}, 30000)\n"},
		{"python3", "import time\ntime.sleep(30)\n"},
		// Started with options before it, the script names its process.
		{"node-titled", "process.title = require('path').basename(__filename)\nsetTimeout(() => {}, 30000)\n"},
	} {
		t.Run(c.interpreter, func(t *testing.T) {
			interpreter, titled := strings.CutSuffix(c.interpreter, "-titled")
			bin, err := exec.LookPath(interpreter)
			if err != nil {
				t.Skipf("%s is not installed", interpreter)
			}
			script := filepath.Join(dir, "myagent-"+c.interpreter)
			if err := os.WriteFile(script, []byte("#!/usr/bin/env "+interpreter+"\n"+c.body), 0o755); err != nil {
				t.Fatal(err)
			}
			runs := map[string]*exec.Cmd{"run by its shebang": exec.Command(script), "run by the interpreter": exec.Command(bin, script)}
			if titled {
				runs = map[string]*exec.Cmd{"run with options before it": exec.Command(bin, "--redirect-warnings", filepath.Join(dir, "w.log"), script)}
			}
			for name, cmd := range runs {
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				// Read it once the interpreter runs the script, as a hook
				// would: Node names its thread only as it starts.
				time.Sleep(500 * time.Millisecond)
				var got string
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					table, err := Lineage(context.Background(), cmd.Process.Pid)
					if err != nil {
						continue
					}
					p, _ := table.Get(cmd.Process.Pid)
					if got = ProgramName(p); got == filepath.Base(script) {
						t.Logf("%s: the kernel names it %q, run as %q", name, p.Comm, p.Args)
						break
					}
				}
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				if got != filepath.Base(script) {
					t.Errorf("%s: ProgramName = %q, want %q", name, got, filepath.Base(script))
				}
			}
		})
	}
}

func TestDeclaredHost(t *testing.T) {
	pt := NewTable([]Process{
		{PID: 1, Comm: "init"},
		{PID: 10, PPID: 1, Comm: "zsh", Args: "-zsh"},
		// A declared agent run by npm's launcher, whose hook runs in a shell.
		{PID: 11, PPID: 10, Comm: "node", Argv: []string{"node", "/usr/local/bin/gemini"}},
		{PID: 12, PPID: 11, Comm: "sh", Args: "sh -c tmag hook gemini"},
		{PID: 13, PPID: 12, Comm: "tmag", Args: "tmag hook gemini"},
		// Its tool runs Claude Code, whose tool names gemini in a hook: the
		// nearest agent is Claude's, whatever runs above it.
		{PID: 14, PPID: 11, Comm: "claude", Args: "claude -p hi"},
		{PID: 15, PPID: 14, Comm: "tmag", Args: "tmag hook gemini"},
		// A hook naming a program nothing above it runs.
		{PID: 20, PPID: 10, Comm: "tmag", Args: "tmag hook gemini"},
		// A declared agent inside another: the nearest one that runs the
		// program named is the host.
		{PID: 30, PPID: 11, Comm: "opencode", Args: "opencode"},
		{PID: 31, PPID: 30, Comm: "tmag", Args: "tmag hook gemini"},
		{PID: 32, PPID: 30, Comm: "tmag", Args: "tmag hook opencode"},
	})
	for _, c := range []struct {
		pid     int
		program string
		host    int
	}{
		{13, "gemini", 11},
		{15, "gemini", 0},
		{20, "gemini", 0},
		{31, "gemini", 11},
		{32, "opencode", 30},
		{13, "zsh", 0},
		{13, "claude", 0},
	} {
		h, ok := pt.DeclaredHost(c.pid, c.program)
		switch {
		case c.host == 0 && ok:
			t.Errorf("DeclaredHost(%d, %s) = %+v, want none", c.pid, c.program, h)
		case c.host != 0 && (!ok || h.Process.PID != c.host || h.Kind != c.program || h.Role != Terminal):
			t.Errorf("DeclaredHost(%d, %s) = %+v, %v, want %d", c.pid, c.program, h, ok, c.host)
		}
	}
}
