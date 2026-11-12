package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/gateway"
)

const (
	claude = "claude"
)

func TestPairingInboxAndRestart(t *testing.T) {
	w := startWorld(t)
	w.spawn(map[string]string{"FAKE_SESSION": "inbox-session", "FAKE_HOOKS": "1"}, filepath.Join(binDir, claude))
	g := w.serve(t)
	credential, code := g.pair()
	if credential.Label != "phone" {
		t.Fatalf("paired with label %q", credential.Label)
	}
	// A code works once.
	if res, _ := g.do(http.MethodPost, "/api/pair", map[string]string{"code": code}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a code paired twice: %s", res.Status)
	}

	s := g.waitSession("inbox-session", func(s gateway.Session) bool { return s.State == "idle" })
	if s.Turn != "" || gateway.Section(s, false) != gateway.SectionSettled {
		t.Fatalf("a session without a turn: %+v %s", s, gateway.Section(s, false))
	}
	g.send(s.ID, "hello")
	s = g.waitIdle(s.ID, "")
	if gateway.Section(s, false) != gateway.SectionInbox {
		t.Fatalf("an unseen finished turn is %s", gateway.Section(s, false))
	}
	key := "local/claude/" + s.ID
	if g.seen()[key] != "" {
		t.Fatalf("a turn was seen before it was opened: %v", g.seen())
	}
	var marked struct {
		Turn string `json:"turn"`
	}
	if res := g.json(http.MethodPost, "/api/seen", map[string]string{"machine": "local", "agent": "claude", "session": s.ID, "turn": s.Turn}, &marked); res.StatusCode != http.StatusOK || marked.Turn != s.Turn {
		t.Fatalf("mark seen: %s %+v", res.Status, marked)
	}
	if got := g.seen()[key]; got != s.Turn {
		t.Fatalf("seen = %q, want %q", got, s.Turn)
	}
	if gateway.Section(s, true) != gateway.SectionSettled {
		t.Fatal("a seen turn is not settled")
	}

	// The seen turn fills no inbox after a gateway restart.
	turn := s.Turn
	g.close()
	g2 := w.serve(t)
	g2.token = g.token
	s2 := g2.waitSession(s.ID, func(s gateway.Session) bool { return s.State == "idle" && s.Turn == turn })
	if got := g2.seen()[key]; got != s.Turn {
		t.Fatalf("seen after restart = %q, want %q", got, s.Turn)
	}
	if gateway.Section(s2, true) != gateway.SectionSettled {
		t.Fatal("a seen turn is not settled after restart")
	}
}

func TestActionAndAnswer(t *testing.T) {
	w := startWorld(t)
	out := filepath.Join(w.dir, "action-out")
	pane := w.spawn(map[string]string{"FAKE_SESSION": "action-session", "FAKE_HOOKS": "1", "FAKE_ANSWER": "1", "FAKE_OUT": out}, filepath.Join(binDir, claude))
	g := w.serve(t)
	g.pair()
	s := g.waitSession("action-session", func(s gateway.Session) bool {
		return s.State == "idle" && contains(s.Capabilities, "send")
	})
	g.send(s.ID, "hello")
	s = g.waitIdle(s.ID, "")
	if text := g.readLast(s.ID); !strings.Contains(text, "answered: hello") {
		t.Fatalf("last reply = %q", text)
	}
	waitFile(t, out, "hello")
	if got := readFile(t, out); strings.Contains(got, "[tmag]") {
		t.Fatalf("the gateway's prompt carries an agent sender line: %q", got)
	}

	w.tmux("send-keys", "-t", pane, "block", "Enter")
	s = g.waitSession(s.ID, func(s gateway.Session) bool { return s.State == "blocked" })
	if len(s.Requests) == 0 {
		t.Fatalf("blocked without a request: %+v", s)
	}
	request := s.Requests[0].ID
	var asked struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if res := g.json(http.MethodGet, "/api/request?machine=local&target="+s.ID+"&request="+request, nil, &asked); res.StatusCode != http.StatusOK {
		t.Fatalf("request: %s", res.Status)
	}
	if asked.Tool != "Bash" || asked.Input["command"] != "date" {
		t.Fatalf("request asks %q %+v", asked.Tool, asked.Input)
	}
	res, b := g.do(http.MethodPost, "/api/act", map[string]string{
		"machine": "local", "verb": "answer", "target": s.ID, "request": request, "decision": "allow",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("answer: %s %s", res.Status, b)
	}
	s = g.waitSession(s.ID, func(s gateway.Session) bool { return s.State == "idle" })
	if text := g.readLast(s.ID); !strings.Contains(text, "answered: block") {
		t.Fatalf("last reply after allow = %q", text)
	}
}

func TestPushAlertsAndVisibility(t *testing.T) {
	w := startWorld(t)
	out := filepath.Join(w.dir, "fake-out")
	pane := w.spawn(map[string]string{
		"FAKE_SESSION": "push-session", "FAKE_HOOKS": "1", "FAKE_ANSWER": "1", "FAKE_OUT": out,
	}, filepath.Join(binDir, claude))
	pushService := newPushService(t)
	g := w.gateway(t, pushService.client())
	g.pair()
	pushService.subscribe(g)

	s := g.waitSession("push-session", func(s gateway.Session) bool { return s.State == "idle" && contains(s.Capabilities, "send") })
	w.tmux("send-keys", "-t", pane, "hello", "Enter")
	s = g.waitIdle(s.ID, "")
	finished := waitPush(t, pushService, "finished")
	if finished.msg.Session != s.ID || finished.msg.Agent != "claude" || finished.msg.Machine != "local" ||
		finished.msg.Title != "claude finished" || finished.headers.Get("Urgency") != "normal" ||
		finished.headers.Get("TTL") == "" || finished.headers.Get("Topic") == "" {
		t.Fatalf("finished push: %+v", finished.msg)
	}
	if strings.Contains(finished.msg.Body, "hello") {
		t.Fatalf("the push body carries a prompt: %q", finished.msg.Body)
	}

	w.tmux("send-keys", "-t", pane, "block", "Enter")
	s = g.waitSession(s.ID, func(s gateway.Session) bool { return s.State == "blocked" })
	if len(s.Requests) == 0 {
		t.Fatalf("blocked without a request: %+v", s)
	}
	blocked := waitPush(t, pushService, "blocked")
	if blocked.msg.Request != s.Requests[0].ID || blocked.msg.Tool != "Bash" || blocked.msg.Title != "claude needs your approval" ||
		blocked.headers.Get("Urgency") != "high" || blocked.headers.Get("TTL") == "" || blocked.headers.Get("Topic") == "" {
		t.Fatalf("blocked push: %+v", blocked.msg)
	}
	if blocked.msg.Dir != filepath.Base(w.dir) {
		t.Fatalf("blocked push names %q, want %q", blocked.msg.Dir, filepath.Base(w.dir))
	}
	for key := range blocked.fields {
		switch key {
		case "title", "body", "tag", "url", "kind", "machine", "agent", "session", "request", "tool", "dir":
		default:
			t.Fatalf("push payload carries %q: %v", key, blocked.fields)
		}
	}
	if strings.Contains(string(mustJSON(blocked.fields)), "date") || strings.Contains(string(mustJSON(blocked.fields)), "hello") {
		t.Fatalf("push payload carries content: %v", blocked.fields)
	}
	// The dialog waits for an answer; close it so the agent can end.
	w.tmux("send-keys", "-t", pane, "yes", "Enter")
	g.waitSession(s.ID, func(s gateway.Session) bool { return s.State == "idle" })
	time.Sleep(300 * time.Millisecond)
	drainPush(pushService)

	// A page that reports itself visible takes the next alert instead.
	if res := g.json(http.MethodPost, "/api/visible", map[string]bool{"visible": true}, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("visible report: %s", res.Status)
	}
	before := s.Turn
	time.Sleep(150 * time.Millisecond)
	w.tmux("send-keys", "-t", pane, "visible turn", "Enter")
	s = g.waitSession(s.ID, func(s gateway.Session) bool {
		return s.State == "idle" && s.Turn != "" && s.Turn != before
	})
	select {
	case p := <-pushService.got:
		t.Fatalf("a visible page got a %s push: %+v", p.msg.Kind, p.msg)
	case <-time.After(2 * time.Second):
	}
}

func TestRevocationClosesStreamAndDropsPush(t *testing.T) {
	w := startWorld(t)
	w.spawn(map[string]string{"FAKE_SESSION": "revoke-session", "FAKE_HOOKS": "1"}, filepath.Join(binDir, claude))
	pushService := newPushService(t)
	g := w.gateway(t, pushService.client())
	credential, _ := g.pair()
	pushService.subscribe(g)
	// The subscribed credential is listed with push on.
	var before struct {
		Credentials []struct {
			ID   string `json:"id"`
			Push bool   `json:"push"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(w.client("auth", "list", "--json", "--state-dir", w.state), &before); err != nil || len(before.Credentials) != 1 || !before.Credentials[0].Push {
		t.Fatalf("auth list before revoke: %+v %v", before, err)
	}

	req, _ := http.NewRequest(http.MethodGet, g.base+"/api/watch", nil)
	req.Header.Set("Authorization", "Bearer "+g.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("watch: %v %v", err, res)
	}
	defer res.Body.Close()
	closed := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, res.Body)
		closed <- err
	}()
	time.Sleep(150 * time.Millisecond) // let the handler register its stream

	w.client("auth", "revoke", credential.ID, "--state-dir", w.state)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("watch stream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation left the page stream open")
	}
	if res, _ := g.do(http.MethodGet, "/api/machines", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked credential still opens the API: %s", res.Status)
	}
	var after struct {
		Credentials []json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal(w.client("auth", "list", "--json", "--state-dir", w.state), &after); err != nil || len(after.Credentials) != 0 {
		t.Fatalf("auth list after revoke: %+v %v", after, err)
	}
}

func waitFile(t *testing.T, path, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never received %q", path, want)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func drainPush(p *pushService) {
	for {
		select {
		case <-p.got:
		default:
			return
		}
	}
}

func waitPush(t *testing.T, p *pushService, kind string) pushRecord {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case record := <-p.got:
			if record.msg.Kind == kind {
				return record
			}
		case <-deadline:
			t.Fatalf("no %s push", kind)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
