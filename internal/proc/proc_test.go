package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAgentKind(t *testing.T) {
	for _, c := range []struct {
		comm, args, want string
	}{
		{"claude", "claude --resume", Claude},
		{"node", "node /opt/homebrew/bin/claude", Claude},
		{"node", "node /usr/local/bin/claude --model haiku", Claude},
		{"node", "node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", Claude},
		{"node", "node /home/me/claudette/index.js", ""},
		{"codex", "codex", Codex},
		{"codex", "codex resume --last", Codex},
		{"node", "node /usr/lib/node_modules/@openai/codex/bin/codex.js", Codex},
		{"codex", "codex app-server --listen unix:// --managed-daemon", ""},
		{"codex", "codex app-server daemon pid-update-loop", ""},
		{"codex-code-mode", "/x/codex-code-mode-host", ""},
		{"node", "node /usr/lib/node_modules/@openai/codex/bin/codex.js app-server", ""},
		// Root options and their values may precede the subcommand.
		{"codex", "codex -c model=example app-server --managed-daemon", ""},
		{"codex", "codex --config=model=example --enable foo -m o3 app-server", ""},
		{"codex", "codex -C /work --yolo exec-server", ""},
		{"codex", "codex -c model=example resume --last", Codex},
		{"codex", "codex -m app-server", Codex}, // a model value, not a subcommand
		// Claude Code 2.1.317's background supervisor and its session hosts.
		{"claude", "/home/me/.local/bin/claude daemon run --origin transient", ""},
		{"claude", "claude bg-pty-host --bg-pty-host /tmp/cc-daemon-1000/x/pty/1.sock 200 50 --", ""},
		{"claude", "claude bg-spare --bg-spare /tmp/cc-daemon-1000/x/spare/1.claim.sock", ""},
		{"claude", "claude attach 21352afc", Claude},
		{"claude", "claude agents", Claude},
		{"2.1.317", "/home/me/.local/share/claude/versions/2.1.317 --session-id s1", ""},
		// Claude Code's npm package runs its native executable as claude.exe.
		{"claude.exe", "/home/me/.local/share/pnpm/global/5/node_modules/@anthropic-ai/claude-code/bin/claude.exe --model haiku", Claude},
		{"claude.exe", "claude.exe", Claude},
		{"claude.exe", "/home/me/.local/share/pnpm/global/5/node_modules/@anthropic-ai/claude-code/bin/claude.exe --session-id s1", Claude},
		{"claude.exe", "/home/me/.local/share/pnpm/global/5/node_modules/@anthropic-ai/claude-code/bin/claude.exe daemon run --origin transient", ""},
		{"claude.exe", "/home/me/.local/share/pnpm/global/5/node_modules/@anthropic-ai/claude-code/bin/claude.exe bg-pty-host --bg-pty-host /tmp/p.sock 200 50 --", ""},
		{"claude.exe", "/home/me/.local/share/pnpm/global/5/node_modules/@anthropic-ai/claude-code/bin/claude.exe bg-spare --bg-spare /tmp/c.sock", ""},
		{"claudette", "claudette", ""},
		// Pi 1.5 renames its process, command line included; before it
		// does, node runs its package's script.
		{"pi", "pi", Pi},
		{"node", "node /home/me/.local/share/pnpm/global/v11/x/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js", Pi},
		{"pi-rpc", "pi-rpc", ""},
		// oh-my-pi 18.18 names its process omp on Linux, from its compiled
		// binary or from bun; ps shows the command it was started as.
		{"omp", "/home/me/.local/bin/omp --model fake/fake", OMP},
		{"omp", "/tmp/omp-linux-x64 --no-session", OMP},
		{"bun", "bun /home/me/.bun/bin/omp", OMP},
		{"bun", "bun /home/me/.bun/install/global/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js", OMP},
		// Installing them is no agent.
		{"bun", "bun install -g @oh-my-pi/pi-coding-agent", ""},
		{"node", "node /usr/bin/npm install -g @earendil-works/pi-coding-agent", ""},
		{"omp", "/home/me/.local/bin/omp --mode rpc", ""},
		{"omp", "/home/me/.local/bin/omp --mode=rpc-ui", ""},
		{"omp", "/home/me/.local/bin/omp acp", ""},
		{"omp", "/home/me/.local/bin/omp --mode json -p hello", OMP},
		{"omp", "/home/me/.local/bin/omp __omp_worker_daemon_broker", ""},
		{"zsh", "-zsh", ""},
	} {
		if got := AgentKind(Process{Comm: c.comm, Args: c.args}); got != c.want {
			t.Errorf("AgentKind(%q, %q) = %q, want %q", c.comm, c.args, got, c.want)
		}
	}
}

// Pi's and oh-my-pi's modes that serve another program host sessions no
// person types into; their helper processes host none.
func TestPiServices(t *testing.T) {
	for _, c := range []struct {
		comm, args, want string
	}{
		{"pi-rpc", "pi-rpc", Pi},
		{"pi", "pi", ""},
		{"omp", "/home/me/.local/bin/omp --mode rpc", OMP},
		{"omp", "/home/me/.local/bin/omp acp", OMP},
		{"omp", "/home/me/.local/bin/omp", ""},
		{"omp", "/home/me/.local/bin/omp __omp_worker_lsp_mux", ""},
		{"omp", "/home/me/.local/bin/omp explain acp", ""},
	} {
		if got := ServiceKind(Process{Comm: c.comm, Args: c.args}); got != c.want {
			t.Errorf("ServiceKind(%q, %q) = %q, want %q", c.comm, c.args, got, c.want)
		}
	}
	if !IsAgent(Pi) || !IsAgent(OMP) || IsAgent("node") || IsAgent("") {
		t.Error("IsAgent does not name exactly the agent kinds")
	}
}

func TestHostOf(t *testing.T) {
	pt := NewTable([]Process{
		{PID: 1, Comm: "init"},
		// An interactive Claude Code in a pane.
		{PID: 10, PPID: 1, Comm: "zsh", Args: "-zsh"},
		{PID: 11, PPID: 10, Comm: "claude", Args: "claude"},
		{PID: 12, PPID: 11, Comm: "sh", Args: "sh -c tmag hook claude"},
		{PID: 13, PPID: 12, Comm: "tmag", Args: "tmag hook claude"},
		// A background session under the supervisor.
		{PID: 20, PPID: 1, Comm: "claude", Args: "claude daemon run --origin transient"},
		{PID: 21, PPID: 20, Comm: "claude", Args: "claude bg-pty-host --bg-pty-host /tmp/x.sock 200 50 --"},
		{PID: 22, PPID: 21, Comm: "2.1.317", Args: "/v/2.1.317 --session-id s2"},
		{PID: 23, PPID: 22, Comm: "tmag", Args: "tmag hook claude"},
		// A Codex thread in the shared daemon.
		{PID: 30, PPID: 1, Comm: "codex", Args: "codex app-server --listen unix:// --managed-daemon"},
		{PID: 31, PPID: 30, Comm: "sh", Args: "/bin/zsh -lc tmag hook codex"},
		// Pi's extension runs the hook; oh-my-pi's in RPC mode, for an editor.
		{PID: 40, PPID: 10, Comm: "pi", Args: "pi"},
		{PID: 41, PPID: 40, Comm: "tmag", Args: "tmag hook pi"},
		{PID: 50, PPID: 1, Comm: "omp", Args: "/usr/local/bin/omp --mode rpc"},
		{PID: 51, PPID: 50, Comm: "tmag", Args: "tmag hook omp"},
	})
	for _, c := range []struct {
		pid  int
		host int
		kind string
		role Role
	}{
		{13, 11, Claude, Terminal},
		{23, 21, Claude, Service},
		{31, 30, Codex, Service},
		{41, 40, Pi, Terminal},
		{51, 50, OMP, Service},
	} {
		h, ok := pt.HostOf(c.pid)
		if !ok || h.Process.PID != c.host || h.Kind != c.kind || h.Role != c.role {
			t.Errorf("HostOf(%d) = %+v, %v", c.pid, h, ok)
		}
	}
	if _, ok := pt.HostOf(10); ok {
		t.Error("a shell has a host")
	}
}

// The chains probe 2 recorded: an interactive client, a background worker
// under its pty host and daemon, and the spare branch. A worker's own argv
// looks like a client's, so HostOf must keep the session on the pty host.
func TestClaudeExeProbeChains(t *testing.T) {
	exe := "/home/me/.local/share/pnpm/store/v11/links/@anthropic-ai/claude-code/2.1.319/x/node_modules/@anthropic-ai/claude-code/bin/claude.exe"
	pt := NewTable([]Process{
		{PID: 1, PPID: 0, Comm: "docker-init"},
		{PID: 10, PPID: 1, Comm: "tmux: server", Args: "tmux -L probe new-session -d -s probe"},
		// Interactive: hook -> sh -> the client, under the tmux server.
		{PID: 11, PPID: 10, Comm: "claude.exe", Argv: []string{exe, "--plugin-dir", "/p", "--permission-mode", "default", "--model", "haiku", "--name", "probe2-interactive"}, Start: "s11"},
		{PID: 12, PPID: 11, Comm: "sh", Args: "/bin/sh -c python3 hook.py", Start: "s12"},
		{PID: 13, PPID: 12, Comm: "python3", Args: "python3 hook.py", Start: "s13"},
		// Background: daemon -> pty host -> worker -> hook. The pty host titles
		// itself as one argv string and carries the worker's whole command after
		// its own flags.
		{PID: 20, PPID: 1, Comm: "claude.exe", Argv: []string{exe, "daemon", "run", "--origin", "transient", "--spawned-by", "{}"}, Start: "s20"},
		{PID: 21, PPID: 20, Comm: "claude.exe", Argv: []string{"claude bg-pty-host", "--bg-pty-host", "/tmp/cc-daemon-1000/a3751f26/pty/5e446214.sock", "200", "50", "--", exe, "--session-id", "5e446214-f00b-4988-b22a-70caaa23273d", "--name", "probe2-background"}, Start: "s21"},
		{PID: 22, PPID: 21, Comm: "claude.exe", Argv: []string{exe, "--session-id", "5e446214-f00b-4988-b22a-70caaa23273d", "--name", "probe2-background"}, Start: "s22"},
		{PID: 23, PPID: 22, Comm: "sh", Args: "/bin/sh -c python3 hook.py", Start: "s23"},
		{PID: 24, PPID: 23, Comm: "python3", Args: "python3 hook.py", Start: "s24"},
		// Hooks directly under the pty host and the daemon test their roles.
		{PID: 26, PPID: 21, Comm: "tmag", Args: "tmag hook claude", Start: "s26"},
		{PID: 28, PPID: 20, Comm: "tmag", Args: "tmag hook claude", Start: "s28"},
		// Spare branch: a second daemon, its spare pty host and the spare worker.
		{PID: 30, PPID: 1, Comm: "claude.exe", Argv: []string{exe, "daemon", "run", "--origin", "transient"}, Start: "s30"},
		{PID: 31, PPID: 30, Comm: "claude.exe", Argv: []string{"claude bg-pty-host", "--bg-pty-host", "/tmp/cc-daemon-1000/a3751f26/spare/a24d02a8.pty.sock", "200", "50", "--", exe, "--bg-spare", "/tmp/cc-daemon-1000/a3751f26/spare/a24d02a8.claim.sock"}, Start: "s31"},
		{PID: 32, PPID: 31, Comm: "claude.exe", Argv: []string{"claude bg-spare", "--bg-spare", "/tmp/cc-daemon-1000/a3751f26/spare/a24d02a8.claim.sock"}, Start: "s32"},
		{PID: 33, PPID: 32, Comm: "tmag", Args: "tmag hook claude", Start: "s33"},
		{PID: 34, PPID: 31, Comm: "tmag", Args: "tmag hook claude", Start: "s34"},
	})
	for _, c := range []struct {
		pid, host int
		kind      string
		role      Role
	}{
		{13, 11, Claude, Terminal}, // an interactive client
		{24, 21, Claude, Service},  // a worker: its pty host hosts the session
		{26, 21, Claude, Service},  // the pty host itself
		{28, 20, Claude, Service},  // the daemon
		{33, 32, Claude, Service},  // the spare worker
		{34, 31, Claude, Service},  // the spare pty host
	} {
		h, ok := pt.HostOf(c.pid)
		if !ok || h.Process.PID != c.host || h.Kind != c.kind || h.Role != c.role {
			t.Errorf("HostOf(%d) = %+v, %v; want %d %s %s", c.pid, h, ok, c.host, c.kind, c.role)
		}
	}
	worker, _ := pt.Get(22)
	if AgentKind(worker) != Claude || ServiceKind(worker) != "" {
		t.Fatalf("the worker is classified as %q/%q, not a client over a service", AgentKind(worker), ServiceKind(worker))
	}
	spare, _ := pt.Get(32)
	if AgentKind(spare) != "" || ServiceKind(spare) != Claude {
		t.Fatalf("the spare worker is classified as %q/%q, not a service", AgentKind(spare), ServiceKind(spare))
	}
	interactive, _ := pt.Get(11)
	if AgentKind(interactive) != Claude || ServiceKind(interactive) != "" {
		t.Fatalf("the interactive client is classified as %q/%q", AgentKind(interactive), ServiceKind(interactive))
	}
}

func TestAliveComparesStart(t *testing.T) {
	pt := NewTable([]Process{{PID: 5, Start: "100"}})
	if !pt.Alive(5, "100") || !pt.Alive(5, "") || pt.Alive(5, "99") || pt.Alive(6, "") {
		t.Fatal("Alive does not tell a reused pid apart")
	}
	self, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	me, ok := self.Get(os.Getpid())
	if !ok || me.Start == "" || !Running(me.PID, me.Start) || Running(me.PID, me.Start+"0") {
		t.Fatalf("own start %q not read back", me.Start)
	}
}

func TestPromptNamingAServiceIsNotAService(t *testing.T) {
	// codex "fix app-server issue": the prompt is one argument.
	p := Process{Comm: "codex", Argv: []string{"/usr/bin/codex", "fix app-server issue"}, Args: "/usr/bin/codex fix app-server issue"}
	if got := AgentKind(p); got != Codex {
		t.Fatalf("AgentKind = %q, want codex", got)
	}
	// Without argument boundaries (ps), a prompt starting with a service name.
	p = Process{Comm: "codex", Args: "/usr/bin/codex app-server is down"}
	if got := AgentKind(p); got != Codex {
		t.Fatalf("space-joined prompt: AgentKind = %q", got)
	}
	p = Process{Comm: "codex", Argv: []string{"/usr/bin/codex", "-c", "k=v", "fix app-server issue"}}
	if got := AgentKind(p); got != Codex {
		t.Fatalf("prompt after an option: AgentKind = %q", got)
	}
	p = Process{Comm: "codex", Args: "/usr/bin/codex -m o3 app-server is down"}
	if got := AgentKind(p); got != Codex {
		t.Fatalf("space-joined prompt after an option: AgentKind = %q", got)
	}
	p = Process{Comm: "codex", Argv: []string{"/usr/bin/codex", "resume", "app-server"}}
	if got := AgentKind(p); got != Codex {
		t.Fatalf("a later argument named a service: AgentKind = %q", got)
	}
}

func TestTreeWalks(t *testing.T) {
	tb := NewTable([]Process{
		{PID: 1, PPID: 0, Comm: "init"},
		{PID: 10, PPID: 1, Comm: "zsh"},
		{PID: 11, PPID: 10, Comm: "claude"},
		{PID: 12, PPID: 11, Comm: "bash"},
		{PID: 13, PPID: 12, Comm: "codex", Args: "codex exec"},
	})
	if got := tb.Ancestors(13); len(got) != 5 || got[0] != 13 || got[4] != 1 {
		t.Fatalf("Ancestors = %v", got)
	}
	if p, kind, ok := tb.FindAgent(10); !ok || p.PID != 11 || kind != Claude {
		t.Fatalf("FindAgent picked %+v %q", p, kind)
	}
	cyclic := NewTable([]Process{{PID: 2, PPID: 3}, {PID: 3, PPID: 2}})
	if got := cyclic.Ancestors(2); len(got) != 2 {
		t.Fatalf("cycle not cut: %v", got)
	}
}

func TestSnapshotFindsSelf(t *testing.T) {
	tb, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.All()) == 0 {
		t.Fatal("empty process table")
	}
}

func TestPSFallbackReadsProcessGroupsAndStart(t *testing.T) {
	ps, err := readPS()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	for _, p := range ps {
		if p.PID == os.Getpid() {
			if p.PGID <= 0 || p.Comm == "" || len(strings.Fields(p.Start)) != 5 || !strings.Contains(p.Args, "proc.test") {
				t.Fatalf("own row parsed as %+v", p)
			}
			return
		}
	}
	t.Fatal("ps did not list this process")
}

func TestLookPathAll(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(dir, "tmag"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "plain"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.ListSeparator)
	got := LookPathAll("/nonexistent"+sep+b+sep+a+sep+b, "tmag")
	if len(got) != 2 || got[0] != filepath.Join(b, "tmag") || got[1] != filepath.Join(a, "tmag") {
		t.Fatalf("LookPathAll = %v", got)
	}
	if got := LookPathAll(a, "plain"); len(got) != 0 {
		t.Fatalf("found a non-executable file: %v", got)
	}
}

func TestEnvironOfChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process environments are only readable on Linux")
	}
	t.Setenv("PATH", "/tmag-test-path")
	t.Setenv("TMAG_SECRET_TEST", "not to be read")
	cmd := exec.Command("/bin/sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// The environment is readable only once the child has executed.
	var got map[string]string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if got, _ = Environ(cmd.Process.Pid, "PATH", "HOME_UNSET_X"); got["PATH"] != "" {
			break
		}
	}
	if len(got) != 1 || got["PATH"] != "/tmag-test-path" {
		t.Fatalf("Environ = %v", got)
	}
}
