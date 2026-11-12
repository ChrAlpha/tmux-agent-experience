package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readEventually waits until path holds every want, as the fake agent appends
// what it took.
func readEventually(t *testing.T, path string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		ok := true
		for _, w := range want {
			if !strings.Contains(string(b), w) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s does not hold %q; got:\n%s", path, want, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// agentSend types a tmag command into a fake agent, as its Bash tool runs it,
// and waits for the agent's turn around it to end.
func agentSend(t *testing.T, h *harness, pane string, args ...string) {
	t.Helper()
	h.tmux("send-keys", "-t", pane, "tmag "+strings.Join(args, " "), "Enter")
	h.waitSession(sidB, "working")
	h.waitSession(sidB, "idle")
}

// A Claude session takes a send through the channel its caller gives it: a
// person's idle prompt is typed into the pane, an agent's or a working
// session's prompt goes through Claude Code's inbox, and --wait starts the
// turn whose reply read --last returns.
func TestSendClaudeThroughItsChannels(t *testing.T) {
	h := start(t)
	f := h.follow()
	outA, outB := filepath.Join(h.dir, "a"), filepath.Join(h.dir, "b")
	a := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": outA, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "1000",
	}, filepath.Join(bin, "claude"))
	b := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "1000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")
	h.waitPane(b, "idle", "process")

	// A person's idle prompt is typed into the pane, named with its server:
	// the person runs tmag outside the test's server.
	if r := h.tmag("", "send", sidA[:8], "hello"); r.code != 0 || !strings.Contains(r.out, "sent to "+sidA[:8]+" ("+h.name+":"+a+") ") {
		t.Fatalf("a person's idle send: %d %s", r.code, r.out)
	}
	h.eventually("A answers the person's prompt", func() bool {
		r := h.tmag("", "read", sidA[:8], "--last")
		return r.code == 0 && r.out == "answered: hello\n"
	})
	f.agrees(h)

	// An agent's idle prompt goes through the inbox and names its sender.
	agentSend(t, h, b, "send", sidA[:8], "peer hello")
	h.eventually("A answers the agent's prompt", func() bool {
		r := h.tmag("", "read", sidA[:8], "--last")
		return r.code == 0 && r.out == "answered: peer hello\n"
	})
	readEventually(t, outA, "inbox claude "+sidB[:8]+" via tmag")
	f.agrees(h)

	// A person's prompt while A works goes through the inbox too.
	if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 || strings.Contains(r.out, "(inbox)") {
		t.Fatalf("a person's idle send: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "working")
	if r := h.tmag("", "send", sidA[:8], "during"); r.code != 0 || !strings.Contains(r.out, "(inbox)") {
		t.Fatalf("a person's working send: %d %s", r.code, r.out)
	}
	readEventually(t, outA, "inbox tmag", "message during")
	h.waitSession(sidA, "idle")
	if r := h.tmag("", "read", sidA[:8], "--last"); r.code != 0 || r.out != "answered: long work\n" {
		t.Fatalf("read --last after a working send: %d %q", r.code, r.out)
	}

	// An agent's prompt while A works reaches it at its next step.
	if r := h.tmag("", "send", sidA[:8], "long work two"); r.code != 0 || strings.Contains(r.out, "(inbox)") {
		t.Fatalf("a person's idle send: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "working")
	agentSend(t, h, b, "send", sidA[:8], "during agent")
	readEventually(t, outA, "inbox claude "+sidB[:8]+" via tmag", "message [tmag] From claude "+sidB+", another agent. No answer needed.", "during agent")
	h.waitSession(sidA, "idle")
	if r := h.tmag("", "read", sidA[:8], "--last"); r.code != 0 || r.out != "answered: long work two\n" {
		t.Fatalf("read --last after an agent's working send: %d %q", r.code, r.out)
	}
	f.agrees(h)

	// --wait from an agent starts A's own turn through the inbox.
	agentSend(t, h, b, "send", sidA[:8], "--wait", "--timeout", "20s", "wait hello")
	h.eventually("A answers the waiting agent", func() bool {
		r := h.tmag("", "read", sidA[:8], "--last")
		return r.code == 0 && r.out == "answered: wait hello\n"
	})
	readEventually(t, outB, "idle")
	f.agrees(h)
}

// Pi and oh-my-pi take the extension's framing through the socket their
// payloads name: an agent's idle prompt starts a turn, and an agent's prompt
// while the target works is read at its next step.
func TestSendPiAndOhMyPiThroughTheirInboxes(t *testing.T) {
	for agent, stop := range map[string]string{"pi": "agent_settled", "omp": "agent_end"} {
		t.Run(agent, func(t *testing.T) {
			h := start(t)
			f := h.follow()
			out := filepath.Join(h.dir, "received")
			outB := filepath.Join(h.dir, "b")
			target := h.spawn(h.dir, map[string]string{
				"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "1000",
			}, filepath.Join(bin, agent))
			b := h.spawn(h.dir, map[string]string{
				"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "1000",
			}, filepath.Join(bin, "claude"))
			h.waitPane(target, "idle", "process")
			h.waitPane(b, "idle", "process")

			// An agent's idle prompt starts the target's turn.
			agentSend(t, h, b, "send", sidA[:8], "hello")
			h.eventually("the target answers the agent's prompt", func() bool {
				r := h.tmag("", "read", sidA[:8], "--last")
				return r.code == 0 && r.out == "answered: hello\n"
			})
			if got := h.eventNames(sidA); !strings.Contains(got, "Send: message_end:working "+stop+":idle") {
				t.Fatalf("events = %s", got)
			}
			f.agrees(h)

			// A person's idle prompt is typed; an agent's later prompt joins
			// the running turn.
			if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 || strings.Contains(r.out, "(inbox)") {
				t.Fatalf("a person's idle send: %d %s", r.code, r.out)
			}
			h.waitSession(sidA, "working")
			agentSend(t, h, b, "send", sidA[:8], "during")
			readEventually(t, out, "message [tmag] From claude "+sidB+", another agent. No answer needed.", "during")
			h.waitSession(sidA, "idle")
			if r := h.tmag("", "read", sidA[:8], "--last"); r.code != 0 || r.out != "answered: long work\n" {
				t.Fatalf("read --last after a working send: %d %q", r.code, r.out)
			}
			f.agrees(h)
		})
	}
}

// The Codex daemon takes a send on its control socket: an idle thread starts
// a turn, and turn/start on an active one joins it.
func TestSendCodexDaemon(t *testing.T) {
	t.Setenv("FAKE_TURN_MS", "1500")
	h := start(t)
	f := h.follow()
	d := h.daemon()
	d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": sidC, "source": "startup"})
	h.waitSession(sidC, "idle")

	if r := h.tmag("", "send", sidC[:8], "hello"); r.code != 0 || !strings.Contains(r.out, "sent to "+sidC[:8]+" (service)") {
		t.Fatalf("send an idle thread: %d %s", r.code, r.out)
	}
	readEventually(t, d.out, sidC+" hello")
	h.eventually("the idle send answers", func() bool {
		r := h.tmag("", "read", sidC[:8], "--last")
		return r.code == 0 && r.out == "answered: hello\n"
	})
	f.agrees(h)

	if r := h.tmag("", "send", sidC[:8], "long work"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitSession(sidC, "working")
	active, _ := h.sessionOf(sidC)
	if r := h.tmag("", "send", sidC[:8], "during"); r.code != 0 || !strings.Contains(r.out, "(service)") {
		t.Fatalf("send an active thread: %d %s", r.code, r.out)
	}
	readEventually(t, d.out, sidC+" during")
	if now, _ := h.sessionOf(sidC); now.Turn != active.Turn {
		t.Fatalf("the send started a turn: %s became %s", active.Turn, now.Turn)
	}
	h.waitSession(sidC, "idle")
	if r := h.tmag("", "read", sidC[:8], "--last"); r.code != 0 || r.out != "answered: long work\n" {
		t.Fatalf("read --last after an active send: %d %q", r.code, r.out)
	}
	f.agrees(h)
}

// A receiver that takes the prompt and drops its answer leaves the outcome
// unknown; only an explicit refusal says nothing was delivered.
func TestSendWithALostAcknowledgement(t *testing.T) {
	h := start(t)
	f := h.follow()
	out, outB := filepath.Join(h.dir, "received"), filepath.Join(h.dir, "b")
	target := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_INBOX_DROP": "1",
	}, filepath.Join(bin, "pi"))
	b := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "1000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(target, "idle", "process")
	h.waitPane(b, "idle", "process")

	agentSend(t, h, b, "send", sidA[:8], "dropped")
	readEventually(t, outB, "unknown")
	if b, _ := os.ReadFile(outB); strings.Contains(string(b), "did not take it") || strings.Contains(string(b), "next:") {
		t.Fatalf("a lost acknowledgement claims too much:\n%s", b)
	}
	h.waitSession(sidA, "idle")
	if r := h.tmag("", "read", sidA[:8], "--last"); r.code != 0 || r.out != "answered: dropped\n" {
		t.Fatalf("the prompt was taken: %d %q", r.code, r.out)
	}
	f.agrees(h)
}

// A reply to a busy asker that serves an inbox is sent at once, not held
// until its turn ends.
func TestReplyReachesABusyAsker(t *testing.T) {
	h := start(t)
	f := h.follow()
	outA, outB := filepath.Join(h.dir, "a"), filepath.Join(h.dir, "b")
	a := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": outA, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "3000",
	}, filepath.Join(bin, "claude"))
	b := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "1000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")
	h.waitPane(b, "idle", "process")

	if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "working")
	agentSend(t, h, b, "reply", sidA+" answer soon")
	readEventually(t, outB, "replied to "+sidA[:8]+" (inbox)")
	readEventually(t, outA, "inbox claude "+sidB[:8]+" via tmag",
		"message [tmag] Reply from claude "+sidB+", another agent, to your prompt. Do not reply to it.", "answer soon")
	h.waitSession(sidA, "idle")
	if r := h.tmag("", "read", sidA[:8], "--last"); r.code != 0 || r.out != "answered: long work\n" {
		t.Fatalf("read --last after a busy reply: %d %q", r.code, r.out)
	}
	f.agrees(h)
}

// A pane-only asker that is busy makes reply wait and retry; when it is idle
// the reply is typed into its pane, with the same first line.
func TestReplyReachesAPaneOnlyAsker(t *testing.T) {
	h := start(t)
	f := h.follow()
	outA, outB := filepath.Join(h.dir, "a"), filepath.Join(h.dir, "b")
	a := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidA, "FAKE_OUT": outA, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "3000",
	}, filepath.Join(bin, "claude"))
	b := h.spawn(h.dir, map[string]string{
		"FAKE_SESSION": sidB, "FAKE_OUT": outB, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "1000",
	}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")
	h.waitPane(b, "idle", "process")

	if r := h.tmag("", "send", sidA[:8], "long work"); r.code != 0 {
		t.Fatalf("send: %d %s", r.code, r.out)
	}
	h.waitSession(sidA, "working")
	agentSend(t, h, b, "reply", sidA+" answer soon")
	readEventually(t, outB, "replied to "+sidA[:8])
	h.waitSession(sidA, "idle")
	h.eventually("the pane-only asker answers the reply", func() bool {
		r := h.tmag("", "read", sidA[:8], "--last")
		return r.code == 0 && r.out == "answered: answer soon\n"
	})
	readEventually(t, outA, "[tmag] Reply from claude "+sidB+", another agent, to your prompt. Do not reply to it.", "answer soon")
	f.agrees(h)
}
