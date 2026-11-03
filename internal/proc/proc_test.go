package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		{"zsh", "-zsh", ""},
	} {
		if got := AgentKind(Process{Comm: c.comm, Args: c.args}); got != c.want {
			t.Errorf("AgentKind(%q, %q) = %q, want %q", c.comm, c.args, got, c.want)
		}
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

func TestPSFallbackReadsProcessGroups(t *testing.T) {
	ps, err := readPS()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	for _, p := range ps {
		if p.PID == os.Getpid() {
			if p.PGID <= 0 || p.Comm == "" {
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
