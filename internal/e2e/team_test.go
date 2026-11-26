package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

// typed has the agent in pane, of session sid, run line as its tool, and
// waits for the turn that runs it to end.
func typed(t *testing.T, h *harness, pane, sid, line string) {
	t.Helper()
	h.tmux("send-keys", "-t", pane, line, "Enter")
	h.waitSession(sid, "working")
	h.waitSession(sid, "idle")
}

// lastReply waits until the reply that ended sid's last turn is the fake's
// answer to a prompt whose last line is line.
func lastReply(t *testing.T, h *harness, sid, line string) {
	t.Helper()
	h.eventually(sid+" answers "+line, func() bool {
		r := h.tmag("", "read", sid[:8], "--last")
		return r.code == 0 && r.out == "answered: "+line+"\n"
	})
}

// owes is what the session owes, as tmag lists it.
func owes(t *testing.T, h *harness, sid string) []eventlog.Obligation {
	t.Helper()
	s, ok := h.sessionOf(sid)
	if !ok {
		t.Fatalf("no session %s", sid)
	}
	return s.Owes
}

// team starts claude agents with inboxes for sessions sids, idle, and
// returns their panes and outputs.
func team(t *testing.T, h *harness, sids ...string) (panes, outs []string) {
	t.Helper()
	for _, sid := range sids {
		out := filepath.Join(h.dir, sid[:2])
		panes = append(panes, h.spawn(h.dir, map[string]string{
			"FAKE_SESSION": sid, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "500",
		}, filepath.Join(bin, "claude")))
		outs = append(outs, out)
	}
	for i, p := range panes {
		h.waitPane(p, "idle", "process")
		h.waitSession(sids[i], "idle")
	}
	return panes, outs
}

// An agent's ask is owed until it is answered: a reply closes the one that
// asked for it, and a reply nobody asked for is refused.
func TestAnAskIsOwedUntilItsReply(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB)
	a, b := panes[0], panes[1]

	typed(t, h, b, sidB, "tmag reply "+sidA+" unasked")
	readEventually(t, outs[1], "tool 3 tmag reply: refused (unasked)",
		"next: answer an ask of send --wait in your final message; to tell "+sidA+" something it did not ask for, use tmag send "+sidA+" -")

	typed(t, h, a, sidA, "tmag send "+sidB+" --reply -- review the diff")
	h.waitSession(sidB, "idle")
	got := owes(t, h, sidB)
	if len(got) != 1 || got[0].Kind != eventlog.OweReply || got[0].Depth != 1 || got[0].To != (eventlog.Actor{Agent: "claude", Session: sidA}) {
		t.Fatalf("B owes %+v", got)
	}
	f.agrees(h)

	typed(t, h, b, sidB, "tmag reply "+sidA+" here it is")
	readEventually(t, outs[1], "tool 0 replied to "+sidA[:8]+" (inbox)")
	readEventually(t, outs[0], "inbox claude "+sidB[:8]+" via tmag")
	lastReply(t, h, sidA, "here it is")
	if got := owes(t, h, sidB); got != nil {
		t.Fatalf("B still owes %+v after its reply", got)
	}
	if !slices.ContainsFunc(h.events(sidB), func(e eventlog.Event) bool {
		return e.Event == eventlog.Owes && e.By != nil && e.By.Session == sidB && e.State != "" && e.Owes == nil
	}) {
		t.Fatalf("no Owes event reports the closed ask: %+v", h.events(sidB))
	}
	h.waitSession(sidA, "idle")
	typed(t, h, b, sidB, "tmag reply "+sidA+" once more")
	readEventually(t, outs[1], "tool 3 tmag reply: refused (unasked): "+sidA+" asked nothing of you that awaits a reply")
	f.agrees(h)
}

// A session that ends owing a reply has tmag tell the agent that asked,
// once, from the end tmag logged: no agent can have tmag tell another of an
// end that did not happen, and running it again tells nobody twice.
func TestASessionThatEndsOwingAReplyTellsItsAsker(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB)
	typed(t, h, panes[0], sidA, "tmag send "+sidB+" --reply -- a long job")
	h.waitSession(sidB, "idle")
	if len(owes(t, h, sidB)) != 1 {
		t.Fatalf("B owes %+v", owes(t, h, sidB))
	}

	h.tmux("kill-pane", "-t", panes[1])
	h.eventually("B ends", func() bool {
		h.tmag("", "prune")
		_, ok := h.sessionOf(sidB)
		return !ok
	})
	readEventually(t, outs[0], "inbox tmag")
	lastReply(t, h, sidA, "[tmag] Notice: claude "+sidB+" ended without answering your prompt. No answer needed.")
	var end eventlog.Event
	for _, e := range h.events(sidB) {
		if e.Ended() {
			end = e
		}
	}
	if len(end.Owes) != 1 || end.Owes[0].To.Session != sidA {
		t.Fatalf("B's end does not say what it owed: %+v", end)
	}
	notices := func() []eventlog.Event {
		var out []eventlog.Event
		for _, e := range h.events(sidA) {
			if e.Event == eventlog.Send && e.Notice != 0 {
				out = append(out, e)
			}
		}
		return out
	}
	if n := notices(); len(n) != 1 || n[0].Notice != end.Seq || n[0].By != nil || n[0].Via != "inbox" {
		t.Fatalf("notices to A: %+v", n)
	}
	h.waitSession(sidA, "idle")
	// The end again, twice at once, and an event that is no end, tell nobody
	// anything.
	var again sync.WaitGroup
	for range 2 {
		again.Go(func() { h.tmag("", "notify", "--ended", fmt.Sprint(end.Seq)) })
	}
	again.Wait()
	typed(t, h, panes[0], sidA, fmt.Sprintf("tmag notify --ended %d", h.events(sidA)[0].Seq))
	if n := notices(); len(n) != 1 {
		t.Fatalf("notices to A after running notify --ended again: %+v", n)
	}
	f.agrees(h)
}

// What an agent owes an asker that ended is closed: no answer reaches it.
func TestAnAskerThatEndsIsOwedNothing(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, _ := team(t, h, sidA, sidB)
	typed(t, h, panes[0], sidA, "tmag send "+sidB+" --reply -- a long job")
	h.waitSession(sidB, "idle")
	h.tmux("kill-pane", "-t", panes[0])
	h.eventually("A ends and B owes it nothing", func() bool {
		h.tmag("", "prune")
		s, ok := h.sessionOf(sidB)
		_, asker := h.sessionOf(sidA)
		return ok && !asker && s.Owes == nil
	})
	f.agrees(h)
}

// An agent that waits for one that waits for it, itself or through others,
// is refused: both would wait until a timeout. The final answer it was
// asked for still closes the ask when its turn ends.
func TestAWaitForAWaiterIsRefused(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB)
	typed(t, h, panes[0], sidA, "tmag send "+sidB+" --wait --timeout 20s -- tmag send "+sidA+" --wait --timeout 5s are you there")
	readEventually(t, outs[1], "tool 3 tmag send: refused (cycle): "+sidA+" waits for your answer",
		"next: ask it with tmag send "+sidA+" --reply - instead, which does not hold your turn, and finish your turn")
	readEventually(t, outs[0], "tool 0 "+sidB[:8]+" idle")
	if got := owes(t, h, sidB); got != nil {
		t.Fatalf("B still owes %+v after the turn A asked for", got)
	}
	if b, _ := os.ReadFile(outs[0]); strings.Contains(string(b), "inbox claude "+sidB[:8]) {
		t.Fatalf("a refused wait was delivered:\n%s", b)
	}
	f.agrees(h)
}

// Work passed on further than @tmag-peer-depth allows is refused, and
// wait --asked --all returns once every agent asked has answered.
func TestAsksArePassedOnBoundedAndJoined(t *testing.T) {
	h := start(t)
	f := h.follow()
	h.tmux("set-option", "-g", "@tmag-peer-depth", "1")
	panes, outs := team(t, h, sidA, sidB, sidC)
	a, b, c := panes[0], panes[1], panes[2]

	typed(t, h, a, sidA, "tmag send "+sidB+" --reply -- pass it on")
	typed(t, h, b, sidB, "tmag send "+sidC+" --reply -- and on")
	readEventually(t, outs[1], "tool 3 tmag send: refused (depth)", "next: do the work yourself, or tell the person what needs another agent")
	if got := owes(t, h, sidC); got != nil {
		t.Fatalf("C owes %+v after a refused ask", got)
	}
	typed(t, h, a, sidA, "tmag send "+sidC+" --reply -- your part")
	typed(t, h, a, sidA, "tmag send "+sidC+" --reply -- and this")

	h.tmux("send-keys", "-t", a, "tmag wait --asked --all --timeout 20s", "Enter")
	h.waitSession(sidA, "working")
	typed(t, h, b, sidB, "tmag reply "+sidA+" b done")
	typed(t, h, c, sidC, "tmag reply "+sidA+" c done")
	if b, _ := os.ReadFile(outs[0]); strings.Contains(string(b), " settled,") {
		t.Fatalf("wait --all returned with two answers of three:\n%s", b)
	}
	typed(t, h, c, sidC, "tmag reply "+sidA+" and that")
	readEventually(t, outs[0], sidB[:8]+" settled,", sidC[:8]+" settled,")
	h.waitSession(sidA, "idle")
	f.agrees(h)
}

// A wait on several sessions names sessions: a pane among them that shows
// none yet is waited for alone.
func TestAWaitOnSeveralNamesSessions(t *testing.T) {
	h := start(t)
	team(t, h, sidA)
	quiet := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB}, filepath.Join(bin, "claude"))
	h.waitPane(quiet, "", "")
	r := h.tmag("", "wait", sidA[:8], h.target(quiet), "--timeout", "2s")
	if r.code != 1 || !strings.Contains(r.out, "shows no session yet") || !strings.Contains(r.out, "next: tmag wait "+h.target(quiet)+" --for idle waits until it shows its session") {
		t.Fatalf("wait on a session and a pane without one: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "wait", sidA[:8], sidA[:8], "--all", "--timeout", "2s"); r.code != 0 || r.out != sidA[:8]+" idle\n"+sidA[:8]+" idle\n" {
		t.Fatalf("wait --all on idle sessions: %d %q", r.code, r.out)
	}
}

// ok fails the test unless r exited 0.
func ok(t *testing.T, r result) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.out)
	}
}

// threads starts a fake Codex daemon with idle threads ids.
func threads(h *harness, ids ...string) *fakeDaemon {
	h.t.Helper()
	d := h.daemon()
	for _, id := range ids {
		d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "source": "startup"})
		h.waitSession(id, "idle")
	}
	return d
}

// Threads of the Codex daemon owe each other through its control socket as
// panes do: a reply settles a reply ask, a turn settles a final one, and a
// thread that ends owing a reply has its asker told.
func TestAsksAcrossTheCodexDaemon(t *testing.T) {
	h := start(t)
	f := h.follow()
	d := threads(h, sidC, sidD)
	ok(t, d.tool(sidC, "send", sidD, "--reply", "an ask"))
	h.waitSession(sidD, "idle")
	if len(owes(t, h, sidD)) != 1 {
		t.Fatalf("D owes %+v", owes(t, h, sidD))
	}
	f.agrees(h)
	ok(t, d.tool(sidD, "reply", sidC, "an answer"))
	h.waitSession(sidC, "idle")
	if got := owes(t, h, sidD); got != nil {
		t.Fatalf("D still owes %+v", got)
	}
	ok(t, d.tool(sidC, "send", sidD, "--wait", "--timeout", "5s", "a final ask"))
	if got := owes(t, h, sidD); got != nil {
		t.Fatalf("the turn ended and D still owes %+v", got)
	}
	f.agrees(h)
	ok(t, d.tool(sidC, "send", sidD, "--reply", "abandoned"))
	h.waitSession(sidD, "idle")
	d.hook(map[string]any{"hook_event_name": "SessionEnd", "session_id": sidD})
	readEventually(t, d.out, "[tmag] Notice: codex "+sidD+" ended without answering your prompt")
	h.waitSession(sidC, "idle")
	f.agrees(h)
}

// Pi and oh-my-pi owe and answer through their inboxes, and their end tells
// the asker.
func TestAsksOfPiAndOhMyPi(t *testing.T) {
	for _, host := range []string{"pi", "omp"} {
		t.Run(host, func(t *testing.T) {
			h := start(t)
			f := h.follow()
			panes, _ := team(t, h, sidA)
			b := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_OUT": filepath.Join(h.dir, "b"), "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "500"}, filepath.Join(bin, host))
			h.waitPane(b, "idle", "process")
			typed(t, h, panes[0], sidA, "tmag send "+sidB+" --reply -- an ask")
			lastReply(t, h, sidB, "an ask")
			typed(t, h, b, sidB, "tmag reply "+sidA+" an answer")
			lastReply(t, h, sidA, "an answer")
			if got := owes(t, h, sidB); got != nil {
				t.Fatalf("%s still owes %+v", host, got)
			}
			f.agrees(h)
			typed(t, h, panes[0], sidA, "tmag send "+sidB+" --reply -- abandoned")
			h.waitSession(sidB, "idle")
			h.tmux("kill-pane", "-t", b)
			h.eventually(host+" ends", func() bool { h.tmag("", "prune"); _, ok := h.sessionOf(sidB); return !ok })
			lastReply(t, h, sidA, "[tmag] Notice: "+host+" "+sidB+" ended without answering your prompt. No answer needed.")
			f.agrees(h)
		})
	}
}

// A busy asker that takes prompts only through its pane is told once it is
// idle, never typed into while it works, and holds up no other asker.
func TestABusyPaneOnlyAskerIsToldOnceIdle(t *testing.T) {
	h := start(t)
	f := h.follow()
	out := filepath.Join(h.dir, "a")
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_TURN_MS": "500"}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")
	panes, outs := team(t, h, sidB, sidC)
	typed(t, h, a, sidA, "tmag send "+sidB+" --reply -- abandoned")
	typed(t, h, panes[1], sidC, "tmag send "+sidB+" --reply -- abandoned too")
	h.waitSession(sidB, "idle")
	ok(t, h.tmag("sleep busy", "send", sidA))
	h.waitSession(sidA, "working")
	h.tmux("kill-pane", "-t", panes[0])
	h.eventually("B ends", func() bool { h.tmag("", "prune"); _, ok := h.sessionOf(sidB); return !ok })
	lastReply(t, h, sidC, "[tmag] Notice: claude "+sidB+" ended without answering your prompt. No answer needed.")
	readEventually(t, outs[1], "inbox tmag")
	f.agrees(h)
	if b, _ := os.ReadFile(out); strings.Contains(string(b), "[tmag] Notice:") {
		t.Fatalf("a notice was typed into a busy pane:\n%s", b)
	}
	h.tmux("send-keys", "-t", a, "/end", "Enter")
	readEventually(t, out, "[tmag] Notice: claude "+sidB+" ended without answering")
	h.waitSession(sidA, "idle")
	f.agrees(h)
}

// A message queued for a working asker does not hold off the reply or the
// notice its own asks invited.
func TestInvitedAnswersPassAQueuedMessage(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB, sidC)
	for _, id := range []string{sidB, sidC} {
		typed(t, h, panes[0], sidA, "tmag send "+id+" --reply -- pending")
		h.waitSession(id, "idle")
	}
	ok(t, h.tmag("sleep busy", "send", sidA))
	h.waitSession(sidA, "working")
	ok(t, h.tmag("for your information", "send", sidA))
	typed(t, h, panes[1], sidB, "tmag reply "+sidA+" invited reply")
	readEventually(t, outs[0], "invited reply")
	h.tmux("kill-pane", "-t", panes[2])
	h.eventually("C ends", func() bool { h.tmag("", "prune"); _, ok := h.sessionOf(sidC); return !ok })
	readEventually(t, outs[0], "[tmag] Notice: claude "+sidC+" ended without answering")
	if s, _ := h.sessionOf(sidA); s.State != "working" {
		t.Fatalf("A took them idle: %+v", s)
	}
	h.tmux("send-keys", "-t", panes[0], "/end", "Enter")
	h.waitSession(sidA, "idle")
	f.agrees(h)
}

// A session owes at most so many answers: an ask beyond them is refused, and
// every one it took stays answerable.
func TestASessionOwesNoMoreAnswersThanItMay(t *testing.T) {
	h := start(t)
	f := h.follow()
	d := threads(h, sidC, sidD)
	for i := range session.MaxOwes {
		ok(t, d.tool(sidC, "send", sidD, "--reply", fmt.Sprintf("ask %d", i)))
		h.waitSession(sidD, "idle")
	}
	if r := d.tool(sidC, "send", sidD, "--reply", "one too many"); r.code != 3 || !strings.Contains(r.out, "refused (owing)") ||
		!strings.Contains(r.out, "next: ask another agent, or wait for "+sidD+"'s answers") {
		t.Fatalf("an ask beyond the bound: %d %s", r.code, r.out)
	}
	f.agrees(h)
	for i := range session.MaxOwes {
		ok(t, d.tool(sidD, "reply", sidC, fmt.Sprintf("answer %d", i)))
		h.waitSession(sidC, "idle")
	}
	if got := owes(t, h, sidD); got != nil {
		t.Fatalf("D still owes %+v", got)
	}
	f.agrees(h)
}

// A wait cycle through a third agent is refused as one between two.
func TestAWaitCycleThroughAThirdAgentIsRefused(t *testing.T) {
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB, sidC)
	typed(t, h, panes[0], sidA, "tmag send "+sidB+" --wait --timeout 10s -- tmag send "+sidC+" --wait --timeout 6s -- tmag send "+sidA+" --wait --timeout 2s round")
	readEventually(t, outs[2], "tool 3 tmag send: refused (cycle)")
	h.waitSession(sidB, "idle")
	h.waitSession(sidC, "idle")
	f.agrees(h)
}

// @tmag-peer-depth off lets work pass on beyond the default, and a value
// that is no number keeps the default.
func TestPeerDepthOffAndUnreadable(t *testing.T) {
	for _, value := range []string{"off", "deep"} {
		t.Run(value, func(t *testing.T) {
			h := start(t)
			f := h.follow()
			d := threads(h, sidA, sidB, sidC, sidD)
			h.tmux("set-option", "-g", "@tmag-peer-depth", value)
			for _, pair := range [][2]string{{sidA, sidB}, {sidB, sidC}, {sidC, sidD}} {
				ok(t, d.tool(pair[0], "send", pair[1], "--reply", "pass it on"))
				h.waitSession(pair[1], "idle")
			}
			r := d.tool(sidD, "send", sidA, "--reply", "and on")
			switch {
			case value == "off":
				ok(t, r)
				h.waitSession(sidA, "idle")
				if got := owes(t, h, sidA); len(got) != 1 || got[0].Depth != 4 {
					t.Fatalf("A owes %+v", got)
				}
			case r.code != 3 || !strings.Contains(r.out, "refused (depth)"):
				t.Fatalf("an ask 4 deep with %q: %d %s", value, r.code, r.out)
			}
			f.agrees(h)
		})
	}
}

// One agent runs many peers, as a person's supervisor of a dozen agents
// does: it starts each in a named window of its own, finds them again with
// list --mine among agents that are not its, and their answers, given all
// at once while it waits for them, each settle its ask once and reach it.
func TestAnAgentRunsManyPeers(t *testing.T) {
	const n = 8
	h := start(t)
	f := h.follow()
	panes, outs := team(t, h, sidA, sidB) // sidB is no peer of sidA's
	hub, hubOut := panes[0], outs[0]
	sids, wouts := make([]string, n), make([]string, n)
	for i := range n {
		sids[i] = fmt.Sprintf("%08x-0000-7000-8000-%012x", 0xd0+i, 0xd0+i)
		wouts[i] = filepath.Join(h.dir, fmt.Sprintf("w%d", i))
		startable(h, map[string]string{"FAKE_SESSION": sids[i], "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_TURN_MS": "300", "FAKE_OUT": wouts[i]})
		line := fmt.Sprintf("tmag start claude --name w-%d --reply --timeout 20s -- stand by", i)
		if i == 0 {
			line = "tmag start claude --name w-0 --timeout 20s" // no prompt: only the Start event says who started it
		}
		typed(t, h, hub, sidA, line)
		readEventually(t, hubOut, "tool 0 started claude "+sids[i][:8]+" in %")
		if i == 0 {
			h.waitSession(sids[0], "idle")
			f.agrees(h)
			typed(t, h, hub, sidA, "tmag send w-0 --reply -- stand by")
		}
	}
	for _, sid := range sids {
		h.waitSession(sid, "idle")
		if got := owes(t, h, sid); len(got) != 1 || got[0].To.Session != sidA {
			t.Fatalf("%s owes %+v", sid, got)
		}
	}
	f.agrees(h)

	typed(t, h, hub, sidA, "tmag list --mine")
	b, _ := os.ReadFile(hubOut)
	mine := string(b)[strings.LastIndex(string(b), "tool 0 TARGET"):]
	for _, sid := range sids {
		if !strings.Contains(mine, sid[:8]+" (started by you, owes you)") {
			t.Fatalf("list --mine does not mark %s as the hub's:\n%s", sid[:8], mine)
		}
	}
	if !strings.Contains(mine, sidA[:8]+" (you)") || strings.Contains(mine, sidB[:8]) {
		t.Fatalf("list --mine is not the hub's own part:\n%s", mine)
	}
	if r := h.tmag("", "list"); r.code != 0 || strings.Count(r.out, "(started by "+sidA[:8]+")") != n {
		t.Fatalf("a person's list does not say who started the peers: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "list", "--mine"); r.code != 2 || !strings.Contains(r.out, "tmag proves none of yours") {
		t.Fatalf("a person's list --mine: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "inspect", sids[3][:8]); r.code != 0 || !strings.Contains(r.out, "started by: claude "+sidA+"\nowes: claude "+sidA+" (reply, depth 1)\n") {
		t.Fatalf("inspect of a peer: %d %s", r.code, r.out)
	}

	// The hub waits; every peer answers at once.
	h.tmux("send-keys", "-t", hub, "tmag wait --asked --all --timeout 60s", "Enter")
	h.waitSession(sidA, "working")
	for i := range n {
		h.tmux("send-keys", "-t", h.tmux("display-message", "-p", "-t", "w-"+strconv.Itoa(i), "#{pane_id}"), "tmag reply "+sidA+" done-"+strconv.Itoa(i), "Enter")
	}
	h.waitSession(sidA, "idle")
	for i, sid := range sids {
		readEventually(t, wouts[i], "tool 0 replied to "+sidA[:8])
		readEventually(t, hubOut, "message [tmag] Reply from claude "+sid+", another agent, to your prompt. Do not reply to it.\ndone-"+strconv.Itoa(i))
		if o := owes(t, h, sid); o != nil {
			t.Fatalf("%s still owes %+v after its reply", sid, o)
		}
		closed := 0
		for _, e := range h.events(sid) {
			if e.Event == eventlog.Owes && e.By != nil && e.By.Session == sid {
				closed++
			}
		}
		if closed != 1 {
			t.Fatalf("%s settled its ask %d times", sid, closed)
		}
	}
	// Each settled as it replied, in a turn of its own that may still run.
	readEventually(t, hubOut, "tool 0 "+sids[0][:8]+" settled")
	b, _ = os.ReadFile(hubOut)
	waited := string(b)[strings.LastIndex(string(b), "tool 0 "+sids[0][:8]+" settled"):]
	waited, _, _ = strings.Cut(waited, "\n")
	for _, sid := range sids {
		if !strings.Contains(waited, sid[:8]+" settled, ") {
			t.Fatalf("wait --asked --all ended before %s settled: %s", sid[:8], waited)
		}
	}
	f.agrees(h)

	data, _ := os.ReadFile(filepath.Join(h.state, "hook.log"))
	slow := 0
	for line := range strings.Lines(string(data)) {
		if strings.HasSuffix(line, " slow\n") {
			slow++
			continue
		}
		t.Errorf("a hook failed: %s", line)
	}
	t.Logf("%d agents, %d slow hooks", n+2, slow)
}
