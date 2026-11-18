package e2e

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

// screenOf is the digest of what pane shows, as read --lines --json gives
// it to the person.
func screenOf(t *testing.T, h *harness, pane string) string {
	t.Helper()
	r := h.tmag("", "read", pane, "--lines", "20", "--json")
	var doc struct {
		Screen string `json:"screen"`
	}
	if r.code != 0 || json.Unmarshal([]byte(strings.SplitN(r.out, "\n", 2)[0]), &doc) != nil || !strings.HasPrefix(doc.Screen, "20:") || len(doc.Screen) != 67 {
		t.Fatalf("read --json: %d %s", r.code, r.out)
	}
	return doc.Screen
}

// The person answers what tmag does not carry by typing into the pane, only
// while it shows the screen they read; no agent may, and the log keeps how
// much was typed, never what.
func TestThePersonTypesOnlyIntoTheScreenTheyRead(t *testing.T) {
	h := start(t)
	f := h.follow()
	out := filepath.Join(h.dir, "a")
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_ANSWER": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")
	panes, outs := team(t, h, sidB)
	pane := h.target(a)

	ok(t, h.tmag("block", "send", sidA[:8]))
	h.waitSession(sidA, "blocked")
	if r := h.tmag("", "keys", pane, "--screen", "20:"+strings.Repeat("0", 64), "--text", "yes", "Enter"); r.code != 3 ||
		!strings.Contains(r.out, "refused (changed)") || !strings.Contains(r.out, "next: see what it shows now with tmag read "+pane+" --lines 40 --json") {
		t.Fatalf("keys for another screen: %d %s", r.code, r.out)
	}
	if buffers := h.tmux("list-buffers", "-F", "#{buffer_name}"); strings.Contains(buffers, "tmag-") {
		t.Fatalf("a refused paste left its text in a buffer: %q", buffers)
	}
	if r := h.tmag("", "keys", pane, "--screen", strings.Repeat("0", 64), "Enter"); r.code != 2 || !strings.Contains(r.out, "--screen takes the screen a read") {
		t.Fatalf("keys for no screen a read gave: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "keys", pane, "--screen", screenOf(t, h, pane), "rm"); r.code != 2 || !strings.Contains(r.out, `"rm" is none of`) {
		t.Fatalf("a key tmag does not press: %d %s", r.code, r.out)
	}
	typed(t, h, panes[0], sidB, "tmag keys "+a+" --screen "+screenOf(t, h, pane)+" Enter")
	readEventually(t, outs[0], "refused (agent)", "next: only the person types into a pane")

	ok(t, h.tmag("", "keys", pane, "--screen", screenOf(t, h, pane), "--text", "yes", "Enter"))
	readEventually(t, out, "terminal:yes")
	h.waitSession(sidA, "idle")
	i := slices.IndexFunc(h.events(sidA), func(e eventlog.Event) bool { return e.Event == eventlog.Keys })
	if i < 0 {
		t.Fatalf("no Keys event: %+v", h.events(sidA))
	}
	if ev := h.events(sidA)[i]; ev.By != nil || ev.Keys != 1 || ev.Bytes != 3 || ev.Pane == nil || ev.Pane.ID != a {
		t.Fatalf("Keys event = %+v", ev)
	}
	f.agrees(h)
}

// A pane whose agent shows no session yet, at a trust dialog say, takes the
// person's keys too.
func TestKeysReachAPaneWithoutASession(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "a")
	a := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out}, filepath.Join(bin, "claude"))
	h.waitPane(a, "", "")
	pane := h.target(a)
	ok(t, h.tmag("", "keys", pane, "--screen", screenOf(t, h, pane), "--text", "trust", "Enter"))
	readEventually(t, out, "trust")
}
