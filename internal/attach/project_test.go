package attach

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/sockettest"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// startTmuxPane starts a private server and returns a pane in it and the
// program it runs.
func startTmuxPane(t *testing.T) (tmux.Server, string, string) {
	t.Helper()
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("TMUX_TMPDIR", sockettest.Dir(t))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	name := "guard"
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(tmuxPath, append([]string{"-L", name}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("-f", "/dev/null", "new-session", "-d", "-s", "main", "/bin/sh", "-c", "sleep 60")
	t.Cleanup(func() {
		_ = exec.Command(tmuxPath, "-L", name, "kill-server").Run()
	})
	socket := run("display-message", "-p", "#{socket_path}")
	pane := run("list-panes", "-a", "-F", "#{pane_id}")
	var cmd string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cmd = run("display-message", "-p", "-t", pane, "#{pane_current_command}"); cmd == "sleep" {
			break
		}
	}
	return tmux.Server{Socket: socket}, pane, cmd
}

func TestGuardPredicates(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	set := func(agent, id, pid, start, command, state string) {
		t.Helper()
		if err := srv.SetPaneOptions(ctx, pane, map[string]string{
			"agent": agent, "session": id, "pid": pid, "start": start, "evidence": Process, "cmd": command,
			"state": state, "kind": "old", "since": "111",
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	entry := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	rec := session.Record{Agent: "claude", ID: "session"}
	projection := func() string {
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{@tmag_state}|#{@tmag_kind}|#{@tmag_since}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	// The right pane accepts and writes the projection.
	set("claude", "session", "123", "12345", cmd, "idle")
	if got, out := guardedWrite(ctx, entry, rec, Projection{State: "working"}, false, nil); out != wrote || projection() != "working||" || got.Pane.ID != pane {
		t.Fatalf("the full guard refused: projection=%q", projection())
	}

	// Each predicate alone must refuse without touching the projection. A
	// pane whose options name another attachment holds the listed one no
	// more; one that holds it but runs another program may show it again.
	for _, tc := range []struct {
		name                           string
		agent, id, pid, start, command string
		want                           outcome
	}{
		{"foreign agent", "codex", "session", "123", "12345", cmd, gone},
		{"foreign session", "claude", "other", "123", "12345", cmd, gone},
		{"other pid", "claude", "session", "999", "12345", cmd, gone},
		{"other start", "claude", "session", "123", "99999", cmd, gone},
		{"another program", "claude", "session", "123", "12345", "zsh", unwritten},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set(tc.agent, tc.id, tc.pid, tc.start, tc.command, "idle")
			if _, out := guardedWrite(ctx, entry, rec, Projection{State: "working"}, false, nil); out != tc.want {
				t.Fatalf("outcome %d, want %d", out, tc.want)
			}
			if got := projection(); got != "idle|old|111" {
				t.Fatalf("projection changed on refusal: %q", got)
			}
		})
	}

	// A dead pane refuses too, even when every option still matches.
	set("claude", "session", "123", "12345", cmd, "idle")
	if _, err := srv.Run(ctx, "set-window-option", "-t", "main:0", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Run(ctx, "respawn-pane", "-k", "-t", pane, "/bin/true"); err != nil {
		t.Fatal(err)
	}
	dead := ""
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{pane_dead}")
		if err != nil {
			t.Fatal(err)
		}
		dead = strings.TrimSpace(string(out))
		if dead == "1" {
			break
		}
	}
	if dead != "1" {
		t.Fatalf("pane is not dead: %q", dead)
	}
	set("claude", "session", "123", "12345", "true", "idle")
	if _, out := guardedWrite(ctx, entry, rec, Projection{State: "working"}, false, nil); out != unwritten {
		t.Fatalf("a dead pane that still holds the attachment: outcome %d", out)
	}
	if got := projection(); got != "idle|old|111" {
		t.Fatalf("projection changed on a dead pane: %q", got)
	}
}

func TestValuesWithTmuxSyntaxAreNeverUsed(t *testing.T) {
	for _, s := range []session.Shown{
		{Server: "/tmp/tmux.sock", Pane: "%1", PID: 10, Start: "x}#{run-shell,touch /tmp/pwn}"},
		{Server: "/tmp/tmux.sock", Pane: "%x", PID: 10, Start: "10"},
		{Server: "/tmp/tmux.sock", Pane: "%1", PID: 0, Start: "10"},
	} {
		if _, out := guardedWrite(context.Background(), s, session.Record{Agent: "claude", ID: "session"}, Projection{State: "working"}, false, nil); out != unwritten {
			t.Fatalf("entry %+v was used: %d", s, out)
		}
	}
	if _, out := guardedWrite(context.Background(), session.Shown{Server: "/tmp/tmux.sock", Pane: "%1", PID: 10, Start: "10"},
		session.Record{Agent: "claude", ID: "bad id #{}"}, Projection{State: "working"}, false, nil); out != unwritten {
		t.Fatal("a session id with tmux syntax was used")
	}
}

// Project writes the latest state to the panes the record lists, reads the
// options it is asked for with them, and unlists a pane tmux shows to hold
// the attachment no more: closed, or on a server that is gone. A pane that
// may still hold it stays listed.
func TestProjectWritesListedPanesAndUnlistsGoneOnes(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	if _, err := srv.Run(ctx, "set-option", "-g", "@tmag-alert", "finished"); err != nil {
		t.Fatal(err)
	}
	attach := func(pane, pid, start, cmd string) {
		t.Helper()
		if err := srv.SetPaneOptions(ctx, pane, map[string]string{
			"agent": proc.Claude, "session": "s1", "pid": pid, "start": start, "evidence": Process, "cmd": cmd, "state": "working",
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	attach(pane, "123", "12345", cmd)
	out, err := srv.Run(ctx, "new-window", "-d", "-P", "-F", "#{pane_id}", "/bin/sh", "-c", "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	behind := strings.TrimSpace(string(out))
	attach(behind, "125", "12347", "another-program")
	dir := t.TempDir()
	env := Env{Log: eventlog.Open(dir), Store: session.Store{Dir: dir}}
	listed := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	stale := session.Shown{Server: srv.Socket, Pane: behind, PID: 125, Start: "12347"}
	closed := session.Shown{Server: srv.Socket, Pane: "%99", PID: 124, Start: "12346"}
	serverless := session.Shown{Server: sockettest.Dir(t) + "/gone", Pane: "%1", PID: 126, Start: "12348"}
	old := session.Record{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Shown: []session.Shown{listed, stale, closed, serverless}}
	latest := old
	latest.Main, latest.Since = adapter.Idle, time.Unix(200, 0).UTC()
	if err := env.Store.Put(ctx, latest); err != nil {
		t.Fatal(err)
	}

	written, missed, err := env.Project(ctx, old, "@tmag-alert")
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || !written[0].Newer || !written[0].Read || written[0].Options["@tmag-alert"] != "finished" {
		t.Fatalf("written %+v", written)
	}
	if got := written[0].Pane.Options["state"]; got != adapter.Idle {
		t.Fatalf("an older hook wrote its own state, %q, over the latest", got)
	}
	if len(missed) != 3 {
		t.Fatalf("missed %+v", missed)
	}
	stored, _, err := env.Store.Get(ctx, proc.Claude, "s1")
	if err != nil || !stored.Lists(listed) || !stored.Lists(stale) || stored.Lists(closed) || stored.Lists(serverless) {
		t.Fatalf("listed after Project: %+v, %v", stored.Shown, err)
	}

	// A tmux that cannot run proves nothing: every pane stays listed.
	t.Setenv("PATH", t.TempDir())
	latest = stored
	latest.Main = adapter.Working
	if err := env.Store.Put(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if written, missed, err := env.Project(ctx, latest); err != nil || len(written) != 0 || len(missed) != 2 {
		t.Fatalf("without tmux: written %+v, missed %+v, %v", written, missed, err)
	}
	if after, _, err := env.Store.Get(ctx, proc.Claude, "s1"); err != nil || !slices.Equal(after.Shown, stored.Shown) {
		t.Fatalf("listed without tmux: %+v, %v", after.Shown, err)
	}
}

// A Codex pane is written only while its title shows the session, as Check
// judges it: a client that went on to another thread keeps its attachment
// for a full pass to move, but takes no projection of the thread it left.
func TestProjectWritesACodexPaneOnlyWhileItsTitleShowsTheSession(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	const a, b = "019a0000-0000-7000-8000-a00000000000", "019a0000-0000-7000-8000-b00000000000"
	if err := srv.SetPaneOptions(ctx, pane, map[string]string{
		"agent": proc.Codex, "session": a, "pid": "123", "start": "12345", "evidence": TitleID, "cmd": cmd, "state": "idle",
	}, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := Env{Log: eventlog.Open(dir), Store: session.Store{Dir: dir}}
	entry := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	rec := session.Record{Agent: proc.Codex, ID: a, Main: adapter.Working, Shown: []session.Shown{entry}}
	if err := env.Store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	state := func() string {
		t.Helper()
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{@tmag_state}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, tc := range []struct {
		title string
		want  string
	}{
		{"other | repo | " + b[:29] + "...", "idle"},
		{"repo", "idle"},
		{"⠋ work | repo | " + a[:29] + "...", "working"},
	} {
		if _, err := srv.Run(ctx, "select-pane", "-t", pane, "-T", tc.title); err != nil {
			t.Fatal(err)
		}
		if _, _, err := env.Project(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != tc.want {
			t.Fatalf("title %q: pane state %q, want %q", tc.title, got, tc.want)
		}
		if stored, _, _ := env.Store.Get(ctx, proc.Codex, a); !stored.Lists(entry) {
			t.Fatalf("title %q unlisted a pane that still holds the attachment", tc.title)
		}
	}

	// The title changes between Project's read of it and its write: the
	// write's own guard refuses, for a title-id attachment and for a process
	// attachment whose title showed no thread.
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrap := t.TempDir()
	flip := filepath.Join(wrap, "title")
	script := "#!/bin/sh\nif [ \"$4 $8\" = 'set-option @tmag_title' ] && [ -s '" + flip + "' ]; then '" + real + "' -S \"$3\" select-pane -t " + pane +
		" -T \"$(cat '" + flip + "')\"; fi\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrap+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		evidence, before, after string
		written                 bool
	}{
		{TitleID, "repo | " + a[:29] + "...", "repo | " + b[:29] + "...", false},
		// A name that mentions the thread's id is no id item.
		{TitleID, "repo | " + a[:29] + "...", "Investigate " + a[:29] + "... | repo | " + b[:29] + "...", false},
		{TitleID, "repo | " + a[:29] + "...", "调查 " + a[:29] + "... | repo | " + b[:29] + "...", false},
		{TitleID, "repo | " + a[:29] + "...", "name| " + a[:29] + "... | repo | " + b[:29] + "...", false},
		// A spinner's turn is read and judged again, and written.
		{TitleID, "⠋ work | repo | " + a[:29] + "...", "⠙ work | repo | " + a[:29] + "...", true},
		{Process, "repo", "repo | " + b[:29] + "...", false},
		{Process, "repo", "repo |  " + b[:29] + "...", false},
		// A name that mentions an id is none for a process attachment either.
		{Process, "Investigate " + b[:29] + "...", "Investigate " + b[:29] + "...", true},
	} {
		if err := srv.SetPaneOptions(ctx, pane, map[string]string{"evidence": tc.evidence, "state": "idle"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(flip, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.Run(ctx, "select-pane", "-t", pane, "-T", tc.before); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(flip, []byte(tc.after), 0o600); err != nil {
			t.Fatal(err)
		}
		written, missed, err := env.Project(ctx, rec)
		if err != nil || (len(written) == 1) != tc.written || len(written)+len(missed) != 1 {
			t.Fatalf("%s attachment, title %q then %q: written %+v, missed %+v, %v", tc.evidence, tc.before, tc.after, written, missed, err)
		}
		if want := map[bool]string{false: "idle", true: "working"}[tc.written]; state() != want {
			t.Fatalf("%s attachment, title %q then %q: pane state %q, want %q", tc.evidence, tc.before, tc.after, state(), want)
		}
	}
}

// Project reads the record under the pane's lock: a state saved while
// another tmag holds that lock is the one written, not the state Project
// was called with.
func TestProjectReadsTheRecordUnderThePaneLock(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	if err := srv.SetPaneOptions(ctx, pane, map[string]string{
		"agent": proc.Claude, "session": "s1", "pid": "123", "start": "12345", "evidence": Process, "cmd": cmd, "state": "idle",
	}, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := Env{Log: eventlog.Open(dir), Store: session.Store{Dir: dir}}
	entry := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	working := session.Record{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Shown: []session.Shown{entry}}
	if err := env.Store.Put(ctx, working); err != nil {
		t.Fatal(err)
	}
	done := make(chan []Projected, 1)
	if err := Lock(ctx, dir, srv.Socket, pane, func() error {
		go func() {
			written, _, _ := env.Project(ctx, working)
			done <- written
		}()
		time.Sleep(100 * time.Millisecond) // Project waits for the lock meanwhile
		blocked := working
		blocked.Main = adapter.Idle
		return env.Store.Put(ctx, blocked)
	}); err != nil {
		t.Fatal(err)
	}
	written := <-done
	if len(written) != 1 || !written[0].Newer || written[0].Pane.Options["state"] != adapter.Idle {
		t.Fatalf("Project wrote %+v, not the record saved under the lock", written)
	}
}

// Whatever a title holds, the write stores it as a value and compares it as
// one: tmux expands and runs nothing of it, and the pane is written exactly
// when TitleIDs reads the session's thread in it.
func TestAnyTitleIsStoredAndComparedAsAValue(t *testing.T) {
	srv, pane, cmd := startTmuxPane(t)
	ctx := context.Background()
	if err := srv.SetPaneOptions(ctx, pane, map[string]string{
		"agent": proc.Codex, "session": threadA, "pid": "123", "start": "12345", "evidence": TitleID, "cmd": cmd,
	}, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := Env{Log: eventlog.Open(dir), Store: session.Store{Dir: dir}}
	entry := session.Shown{Server: srv.Socket, Pane: pane, PID: 123, Start: "12345"}
	a, b := shown(threadA), shown(threadB)
	for i, title := range []string{
		"proj | " + a,
		"⠼ 运行 echo hi | proj | " + a,
		" " + a + " ",
		"proj | " + a + ";",
		"x ; set-option -g @tmag_pwned 1 ; " + a,
		"#{pane_id} | " + a,
		"a}b,c | " + a,
		"{",
		"\\;",
		"-t | " + a,
		"see " + a + " | proj",
		"proj | " + b,
		"",
	} {
		rec := session.Record{Agent: proc.Codex, ID: threadA, Main: adapter.Working, Since: time.Unix(int64(1000+i), 0).UTC(), Shown: []session.Shown{entry}}
		if err := env.Store.Put(ctx, rec); err != nil {
			t.Fatal(err)
		}
		setTitle(t, srv, pane, title)
		written, _, err := env.Project(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Contains(TitleIDs(title), threadA[:titleIDLen])
		if (len(written) == 1) != want {
			t.Errorf("title %q: written %v, TitleIDs reads the thread %v", title, len(written) == 1, want)
		}
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{@tmag_title}\t#{@tmag_pwned}")
		if err != nil {
			t.Fatal(err)
		}
		stored, pwned, _ := strings.Cut(strings.TrimRight(string(out), "\n"), "\t")
		if pwned != "" || (want && stored != title+".") {
			t.Errorf("title %q: stored %q, ran %q", title, stored, pwned)
		}
	}
}

// setTitle sets the pane's title as a program in it does, through its
// terminal, so that tmux takes it as text.
func setTitle(t *testing.T, srv tmux.Server, pane, title string) {
	t.Helper()
	ctx := context.Background()
	out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{pane_tty}")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSpace(string(out)), []byte("\x1b]2;"+title+"\x1b\\"), 0); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		out, err := srv.Run(ctx, "display-message", "-p", "-t", pane, "#{pane_title}")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimRight(string(out), "\n") == title {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("title %q not set: %q", title, out)
		}
	}
}
