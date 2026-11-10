package hook

import (
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestInboxOf(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(name string) string { return vals[name] }
	}
	claude := map[string]string{claudeInboxVar: "uds:/tmp/claude.sock"}
	for _, c := range []struct {
		name  string
		agent string
		p     adapter.Payload
		get   func(string) string
		want  string
	}{
		{"claude exports it", proc.Claude, adapter.Payload{}, env(claude), "/tmp/claude.sock"},
		{"claude ignores a payload", proc.Claude, adapter.Payload{Inbox: "/tmp/pi.sock"}, env(claude), "/tmp/claude.sock"},
		{"pi sends it", proc.Pi, adapter.Payload{Inbox: "/tmp/pi.sock"}, env(claude), "/tmp/pi.sock"},
		{"omp sends it", proc.OMP, adapter.Payload{Inbox: "/tmp/omp.sock"}, env(nil), "/tmp/omp.sock"},
		{"codex has none", proc.Codex, adapter.Payload{Inbox: "/tmp/x.sock"}, env(claude), ""},
		{"empty", proc.Claude, adapter.Payload{}, env(nil), ""},
		{"relative", proc.Pi, adapter.Payload{Inbox: "pi.sock"}, env(nil), ""},
		{"too long", proc.Claude, adapter.Payload{}, env(map[string]string{claudeInboxVar: "/" + strings.Repeat("s", adapter.MaxPath)}), ""},
	} {
		if got := inboxOf(c.agent, c.p, c.get); got != c.want {
			t.Errorf("%s: inbox %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInboxForNamesTheServingProcess(t *testing.T) {
	w := &attach.World{Proc: proc.NewTable([]proc.Process{
		{PID: 1, PPID: 0, Comm: "init"},
		{PID: 10, PPID: 1, Comm: "claude", Start: "s10"},
		{PID: 11, PPID: 10, Comm: "bash", Start: "s11"},
		{PID: 12, PPID: 11, Comm: "tmag", Start: "s12"},
	})}
	want := &session.Inbox{Path: "/tmp/claude.sock", PID: 10, Start: "s10"}
	if got := inboxFor(w, 12, proc.Claude, "/tmp/claude.sock"); !session.SameInbox(got, want) {
		t.Errorf("inbox = %+v, want %+v", got, want)
	}
	if got := inboxFor(w, 12, proc.Codex, "/tmp/codex.sock"); got != nil {
		t.Errorf("a Codex inbox was recorded from a Claude hook: %+v", got)
	}
	if got := inboxFor(w, 12, proc.Claude, ""); got != nil {
		t.Errorf("an inbox without a socket was recorded: %+v", got)
	}

	// A nested agent is not the one that serves the hook above it.
	nested := &attach.World{Proc: proc.NewTable([]proc.Process{
		{PID: 20, PPID: 1, Comm: "pi", Start: "s20"},
		{PID: 21, PPID: 20, Comm: "claude", Start: "s21"},
		{PID: 22, PPID: 21, Comm: "tmag", Start: "s22"},
	})}
	if got := inboxFor(nested, 22, proc.Claude, "/tmp/c.sock"); !session.SameInbox(got, &session.Inbox{Path: "/tmp/c.sock", PID: 21, Start: "s21"}) {
		t.Errorf("nested Claude inbox = %+v", got)
	}
	if got := inboxFor(nested, 22, proc.Pi, "/tmp/p.sock"); !session.SameInbox(got, &session.Inbox{Path: "/tmp/p.sock", PID: 20, Start: "s20"}) {
		t.Errorf("nested Pi inbox = %+v", got)
	}

	// The nearest process running the agent's program serves the inbox,
	// even when it is a worker tmag takes for neither a client nor a service.
	worker := &attach.World{Proc: proc.NewTable([]proc.Process{
		{PID: 30, PPID: 1, Comm: "omp", Args: "omp __omp_worker_1", Start: "s30"},
		{PID: 31, PPID: 30, Comm: "tmag", Start: "s31"},
	})}
	if got := inboxFor(worker, 31, proc.OMP, "/tmp/omp.sock"); !session.SameInbox(got, &session.Inbox{Path: "/tmp/omp.sock", PID: 30, Start: "s30"}) {
		t.Errorf("worker's inbox = %+v", got)
	}
}

// Probe 2's chains: the nearest process running Claude's program serves the
// inbox, which is the worker for a background session, never its pty host or
// daemon.
func TestInboxForProbe2Chains(t *testing.T) {
	exe := "/home/me/.local/share/pnpm/store/v11/links/@anthropic-ai/claude-code/2.1.319/x/node_modules/@anthropic-ai/claude-code/bin/claude.exe"
	w := &attach.World{Proc: proc.NewTable([]proc.Process{
		{PID: 1, PPID: 0, Comm: "docker-init"},
		{PID: 10, PPID: 1, Comm: "tmux: server"},
		{PID: 11, PPID: 10, Comm: "claude.exe", Argv: []string{exe, "--plugin-dir", "/p", "--name", "probe2-interactive"}, Start: "s11"},
		{PID: 12, PPID: 11, Comm: "sh", Args: "/bin/sh -c python3 hook.py", Start: "s12"},
		{PID: 13, PPID: 12, Comm: "python3", Args: "python3 hook.py", Start: "s13"},
		{PID: 20, PPID: 1, Comm: "claude.exe", Argv: []string{exe, "daemon", "run", "--origin", "transient"}, Start: "s20"},
		{PID: 21, PPID: 20, Comm: "claude.exe", Argv: []string{"claude bg-pty-host", "--bg-pty-host", "/tmp/cc/pty/5.sock", "200", "50", "--", exe, "--session-id", "5e44", "--name", "probe2-background"}, Start: "s21"},
		{PID: 22, PPID: 21, Comm: "claude.exe", Argv: []string{exe, "--session-id", "5e44", "--name", "probe2-background"}, Start: "s22"},
		{PID: 23, PPID: 22, Comm: "sh", Args: "/bin/sh -c python3 hook.py", Start: "s23"},
		{PID: 24, PPID: 23, Comm: "python3", Args: "python3 hook.py", Start: "s24"},
		{PID: 26, PPID: 21, Comm: "tmag", Args: "tmag hook claude", Start: "s26"},
		{PID: 28, PPID: 20, Comm: "tmag", Args: "tmag hook claude", Start: "s28"},
		{PID: 30, PPID: 1, Comm: "claude.exe", Argv: []string{exe, "daemon", "run", "--origin", "transient"}, Start: "s30"},
		{PID: 31, PPID: 30, Comm: "claude.exe", Argv: []string{"claude bg-pty-host", "--bg-pty-host", "/tmp/cc/spare/x.pty.sock", "200", "50", "--", exe, "--bg-spare", "/tmp/cc/spare/x.claim.sock"}, Start: "s31"},
		{PID: 32, PPID: 31, Comm: "claude.exe", Argv: []string{"claude bg-spare", "--bg-spare", "/tmp/cc/spare/x.claim.sock"}, Start: "s32"},
		{PID: 33, PPID: 32, Comm: "tmag", Args: "tmag hook claude", Start: "s33"},
		{PID: 34, PPID: 31, Comm: "tmag", Args: "tmag hook claude", Start: "s34"},
	})}
	for _, c := range []struct {
		hook, pid int
		start     string
	}{
		{13, 11, "s11"}, // an interactive client
		{24, 22, "s22"}, // the worker, not its pty host
		{26, 21, "s21"}, // the pty host
		{28, 20, "s20"}, // the daemon
		{33, 32, "s32"}, // the spare worker
		{34, 31, "s31"}, // the spare pty host
	} {
		got := inboxFor(w, c.hook, proc.Claude, "/tmp/cc.sock")
		if want := (&session.Inbox{Path: "/tmp/cc.sock", PID: c.pid, Start: c.start}); !session.SameInbox(got, want) {
			t.Errorf("inboxFor(%d) = %+v, want %+v", c.hook, got, want)
		}
	}
}

func TestMovedCoversWhatHooksRecord(t *testing.T) {
	cur := session.Record{Agent: proc.Claude, ID: "s1", Cwd: "/w", Host: session.Host{PID: 10, Start: "s10"}, Transcript: "/t.jsonl"}
	base := cur
	base.Inbox = &session.Inbox{Path: "/tmp/claude.sock", PID: 10, Start: "s10"}
	if moved(cur, cur) {
		t.Fatal("nothing changed")
	}
	if !moved(cur, base) {
		t.Fatal("a new inbox was not saved")
	}
	if moved(base, base) {
		t.Fatal("an unchanged inbox was saved")
	}
	for name, change := range map[string]func(*session.Record){
		"cwd":        func(r *session.Record) { r.Cwd = "/elsewhere" },
		"host":       func(r *session.Record) { r.Host.Start = "s11" },
		"transcript": func(r *session.Record) { r.Transcript = "/u.jsonl" },
		"inbox":      func(r *session.Record) { r.Inbox = &session.Inbox{Path: "/tmp/other.sock", PID: 10, Start: "s10"} },
	} {
		next := base
		change(&next)
		if !moved(base, next) {
			t.Errorf("a changed %s was not saved", name)
		}
	}
}
