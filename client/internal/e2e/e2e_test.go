// Package e2e runs the remote companion as people run it: tmag-client serve
// in a window of a tmux server, following a machine through the real tmag,
// with a fake agent in another window. It builds tmag and the fake agent from
// this repository's root module and runs them as external programs.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/webpush/webpushtest"
)

var bin string

func TestMain(m *testing.M) {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		panic(err)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Println("tmux not installed; skipping the client's end-to-end tests")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "tmag-client-e2e")
	if err != nil {
		panic(err)
	}
	bin = dir
	root, _ := filepath.Abs("../../..")
	client, _ := filepath.Abs("../..")
	for _, b := range []struct{ dir, pkg, out string }{
		{root, "./cmd/tmag", "tmag"},
		{root, "./internal/e2e/fakeagent", "claude"},
		{client, "./cmd/tmag-client", "tmag-client"},
	} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, b.out), b.pkg)
		cmd.Dir, cmd.Stdout, cmd.Stderr = b.dir, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			panic(err)
		}
	}
	// Never touch the developer's tmux server.
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type harness struct {
	t      *testing.T
	dir    string
	env    map[string]string // what every window gets
	url    string            // the gateway's local address
	gw     string            // the gateway's pane
	serve  []string
	out    string // the fake agent's record
	agent  string // the fake agent's pane
	client *http.Client
}

const sid = "3c1f2a9e-5b7d-4e8a-9c0f-1d2e3f4a5b6c"
const key = "box/claude/" + sid

// start runs a private tmux server with a fake Claude Code in one window and
// the gateway in another.
func start(t *testing.T, serve ...string) *harness {
	t.Helper()
	sock, err := os.MkdirTemp("/tmp", "tc") // socket paths stay short
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: t.TempDir(), client: &http.Client{Timeout: 2 * time.Minute}}
	h.env = map[string]string{
		"TMUX_TMPDIR":           sock,
		"TMAG_STATE_DIR":        filepath.Join(sock, "state"),
		"TMAG_AUTH_DIR":         filepath.Join(h.dir, "auth"),
		"TMAG_CLIENT_STATE_DIR": filepath.Join(h.dir, "client"),
		"TMAG_BIN":              filepath.Join(bin, "tmag"),
	}
	for k, v := range h.env {
		t.Setenv(k, v)
	}
	server := exec.Command("tmux", "-L", "e2e", "-f", "/dev/null", "new-session", "-d", "-s", "main", "-x", "200", "-y", "50", "/bin/sh")
	server.Env = append(os.Environ(), "SHELL=/bin/sh")
	if out, err := server.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", "e2e", "kill-server").Run()
		os.RemoveAll(sock)
	})
	h.out = filepath.Join(h.dir, "agent.out")
	h.agent = h.spawn(map[string]string{"FAKE_SESSION": sid, "FAKE_HOOKS": "1", "FAKE_INBOX": "1", "FAKE_ANSWER": "1", "FAKE_OUT": h.out}, filepath.Join(bin, "claude"))
	h.serve = serve
	h.startGateway()
	return h
}

func (h *harness) tmux(args ...string) string {
	h.t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", "e2e"}, args...)...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("tmux %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// spawn runs argv in a new window, exec'd as a person's shell runs it.
func (h *harness) spawn(env map[string]string, argv ...string) string {
	h.t.Helper()
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-c", h.dir}
	for _, e := range []map[string]string{h.env, env} {
		for k, v := range e {
			args = append(args, "-e", k+"="+v)
		}
	}
	return h.tmux(append(append(args, "/bin/sh", "-c", `exec "$@"`, "sh"), argv...)...)
}

// startGateway runs tmag-client serve in a window, following this machine
// as box, and waits for it to listen.
func (h *harness) startGateway() {
	h.t.Helper()
	log := filepath.Join(h.dir, fmt.Sprintf("gateway-%d.out", time.Now().UnixNano()))
	args := append([]string{filepath.Join(bin, "tmag-client"), "serve", "--addr", "127.0.0.1:0", "--machine", "box=" + filepath.Join(bin, "tmag")}, h.serve...)
	h.gw = h.spawn(map[string]string{"LOG": log}, append([]string{"/bin/sh", "-c", `exec "$@" >"$LOG" 2>&1`, "sh"}, args...)...)
	h.eventually("the gateway listens", func() bool {
		b, _ := os.ReadFile(log)
		line, _, _ := strings.Cut(string(b), "\n")
		if rest, ok := strings.CutPrefix(line, "tmag-client "); ok {
			if _, at, ok := strings.Cut(rest, " at "); ok {
				h.url, _, _ = strings.Cut(at, ",")
				return true
			}
		}
		return false
	})
	h.t.Cleanup(func() { h.logOnFailure(log) })
}

func (h *harness) logOnFailure(log string) {
	if h.t.Failed() {
		b, _ := os.ReadFile(log)
		h.t.Logf("gateway output:\n%s", b)
	}
}

func (h *harness) stopGateway() {
	h.tmux("send-keys", "-t", h.gw, "C-c")
	h.eventually("the gateway stops", func() bool {
		_, err := http.Get(h.url + "/")
		return err != nil
	})
}

func (h *harness) eventually(what string, ok func() bool) {
	h.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// cli runs tmag-client, as a person in the gateway's state.
func (h *harness) cli(args ...string) string {
	h.t.Helper()
	out, err := exec.Command(filepath.Join(bin, "tmag-client"), args...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("tmag-client %v: %v %s", args, err, out)
	}
	return string(out)
}

// pair pairs a phone through the page's API, with a code auth pair made.
func (h *harness) pair() (token, id string) {
	h.t.Helper()
	var inv struct {
		Invitation struct{ Code string } `json:"invitation"`
	}
	if err := json.Unmarshal([]byte(h.cli("auth", "pair", "phone", "--json")), &inv); err != nil {
		h.t.Fatal(err)
	}
	code, doc := h.do(http.MethodPost, "/api/pair", "", map[string]string{"code": inv.Invitation.Code})
	if code != http.StatusOK {
		h.t.Fatalf("pair: %d %v", code, doc)
	}
	return doc["token"].(string), doc["device"].(map[string]any)["id"].(string)
}

func (h *harness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return resp.StatusCode, doc
}

// page is a page's event stream, with every session it was told of.
type page struct {
	t        *testing.T
	frames   chan frame
	closed   chan struct{}
	sessions map[string]map[string]any
}

type frame struct {
	event string
	data  map[string]any
}

func (h *harness) open(token string) *page {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.url+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		h.t.Fatalf("events: %v %v", err, resp)
	}
	p := &page{t: h.t, frames: make(chan frame, 1024), closed: make(chan struct{}), sessions: map[string]map[string]any{}}
	go func() {
		defer close(p.closed)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 16<<20)
		ev := ""
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				_ = json.Unmarshal([]byte(v), &m)
				p.frames <- frame{ev, m}
			}
		}
	}()
	return p
}

// until reads frames until one of event satisfies ok.
func (p *page) until(what, event string, ok func(map[string]any) bool) map[string]any {
	p.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.event == "session" {
				p.sessions[f.data["key"].(string)] = f.data
			}
			if f.event == event && (ok == nil || ok(f.data)) {
				return f.data
			}
		case <-p.closed:
			p.t.Fatalf("the stream ended waiting for %s", what)
		case <-deadline:
			p.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// drain takes the frames that came, without waiting.
func (p *page) drain() {
	for {
		select {
		case f := <-p.frames:
			if f.event == "session" {
				p.sessions[f.data["key"].(string)] = f.data
			}
		default:
			return
		}
	}
}

// answer answers the session's open request as a person does from the
// page, again when it changed under them: the request a question's
// PreToolUse opens gives way to its PermissionRequest's.
func (h *harness) answer(p *page, token string, body map[string]any) {
	h.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		p.drain()
		if reqs, _ := p.sessions[key]["requests"].([]any); len(reqs) > 0 {
			body["key"], body["request"] = key, reqs[0].(map[string]any)["id"]
			code, doc := h.do(http.MethodPost, "/api/answer", token, body)
			if code == http.StatusOK {
				return
			}
			if e, _ := doc["error"].(map[string]any); code != http.StatusConflict || e["code"] != "refused:changed" {
				h.t.Fatalf("answer: %d %v", code, doc)
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatal("no request took the answer")
		}
	}
}

// session returns the session as the page knows it once ok holds.
func (p *page) session(what string, ok func(map[string]any) bool) map[string]any {
	p.t.Helper()
	p.drain()
	if s := p.sessions[key]; s != nil && ok(s) {
		return s
	}
	return p.until(what, "session", func(m map[string]any) bool { return m["key"] == key && ok(m) })
}

func is(state string, unread bool) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["state"] == state && m["unread"] == unread }
}

func (h *harness) agentGot(line string) {
	h.t.Helper()
	h.eventually("the agent to get "+line, func() bool {
		b, _ := os.ReadFile(h.out)
		for l := range strings.Lines(string(b)) {
			if strings.TrimRight(l, "\n") == line {
				return true
			}
		}
		return false
	})
}

// A device pairs, follows the agent through a turn it prompted, reads the
// reply, and the turn stays read across a restart of the gateway.
func TestATurnFromThePhone(t *testing.T) {
	h := start(t)
	token, _ := h.pair()
	p := h.open(token)
	p.until("hello", "hello", nil)
	p.session("the agent", is("idle", false))
	if code, doc := h.do(http.MethodPost, "/api/send", token, map[string]string{"key": key, "text": "review the diff"}); code != http.StatusOK || doc["via"] == "" {
		t.Fatalf("send: %d %v", code, doc)
	}
	// A person's prompt, with no sender line.
	h.agentGot("review the diff")
	p.session("the turn", is("working", false))
	n := p.until("the finished alert", "alert", func(m map[string]any) bool { return m["kind"] == "finished" })
	if n["title"] != "claude finished" || n["url"] != "/#/s/"+key {
		t.Fatalf("alert %v", n)
	}
	s := p.session("the finished turn", is("idle", true))
	code, doc := h.do(http.MethodGet, "/api/reply?key="+key, token, nil)
	if code != http.StatusOK || doc["text"] != "answered: review the diff" || doc["turn"] != s["turn"] {
		t.Fatalf("reply: %d %v (turn %v)", code, doc, s["turn"])
	}
	code, doc = h.do(http.MethodGet, "/api/screen?lines=50&key="+key, token, nil)
	if code != http.StatusOK || doc["target"] == "" {
		t.Fatalf("screen: %d %v", code, doc)
	}
	if code, _ := h.do(http.MethodPost, "/api/read", token, map[string]any{"key": key, "turn": s["turn"]}); code != http.StatusNoContent {
		t.Fatalf("read: %d", code)
	}
	p.session("the read turn", is("idle", false))

	h.stopGateway()
	h.startGateway()
	q := h.open(token)
	if got := q.session("the session after a restart", func(map[string]any) bool { return true }); got["unread"] != false {
		t.Fatalf("after a restart: %v", got)
	}
}

// A request opened in the agent's dialog shows its tool input on the page,
// and an answer from the page reaches the agent through its hook.
func TestAnswersFromThePhone(t *testing.T) {
	h := start(t)
	token, _ := h.pair()
	p := h.open(token)
	p.session("the agent", is("idle", false))
	h.do(http.MethodPost, "/api/send", token, map[string]string{"key": key, "text": "block"})
	s := p.session("the request", func(m map[string]any) bool { return m["state"] == "blocked" })
	req := s["requests"].([]any)[0].(map[string]any)
	n := p.until("the blocked alert", "alert", func(m map[string]any) bool { return m["kind"] == "blocked" })
	if n["title"] != "claude needs your approval" || n["request"] != req["id"] || n["tool"] != "Bash" {
		t.Fatalf("alert %v", n)
	}
	code, doc := h.do(http.MethodGet, "/api/request?key="+key+"&request="+req["id"].(string), token, nil)
	if code != http.StatusOK || doc["tool"] != "Bash" || doc["input"].(map[string]any)["command"] != "date" {
		t.Fatalf("request: %d %v", code, doc)
	}
	h.answer(p, token, map[string]any{"decision": "allow"})
	h.agentGot("hook:allow")
	p.session("the turn going on", func(m map[string]any) bool { return m["state"] != "blocked" })

	// A question, answered by choice.
	h.do(http.MethodPost, "/api/send", token, map[string]string{"key": key, "text": "ask"})
	s = p.session("the question", func(m map[string]any) bool { return m["state"] == "blocked" && m["kind"] == "question" })
	h.answer(p, token, map[string]any{"decision": "allow", "answers": map[string]string{"Pick a color": "blue"}})
	h.agentGot("hook:allow Pick a color=blue")
	req = s["requests"].([]any)[0].(map[string]any)

	// A refusal comes back with tmag's code, and what to do next.
	code, doc = h.do(http.MethodPost, "/api/answer", token, map[string]any{"key": key, "request": req["id"], "decision": "allow", "answers": map[string]string{"Pick a color": "blue"}})
	if e, _ := doc["error"].(map[string]any); code < 400 || e["code"] == "" {
		t.Fatalf("a closed request answered: %d %v", code, doc)
	}
}

// Alerts reach a subscribed device through its push service when nobody is
// at a terminal and its page is not on screen, with metadata only.
func TestPushReachesAnAbsentDevice(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the gateway trusts the fake push service through SSL_CERT_FILE, which only Linux reads")
	}
	pushes := make(chan []byte, 8)
	push := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		pushes <- b
		w.WriteHeader(http.StatusCreated)
	}))
	defer push.Close()
	certs := filepath.Join(t.TempDir(), "certs.pem")
	if err := os.WriteFile(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: push.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certs)
	h := start(t, "--public", "https://box.example.ts.net", "--push-service", strings.TrimPrefix(push.URL, "https://"))
	token, _ := h.pair()
	p := h.open(token)
	p.session("the agent", is("idle", false))
	_, info := h.do(http.MethodGet, "/api/push", token, nil)
	if info["key"] == nil {
		t.Fatalf("push info %v", info)
	}
	receiver, p256dh, auth := webpushtest.New()
	sub := map[string]any{"endpoint": push.URL + "/push/phone", "keys": map[string]string{"p256dh": p256dh, "auth": auth}}
	if code, doc := h.do(http.MethodPut, "/api/push", token, sub); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %v", code, doc)
	}
	h.do(http.MethodPost, "/api/send", token, map[string]string{"key": key, "text": "review it"})
	var body []byte
	select {
	case body = <-pushes:
	case <-time.After(20 * time.Second):
		t.Fatal("no push")
	}
	plain, err := receiver.Open(body)
	if err != nil {
		t.Fatal(err)
	}
	var n map[string]any
	_ = json.Unmarshal(plain, &n)
	if n["kind"] != "finished" || n["key"] != key || n["title"] != "claude finished" {
		t.Fatalf("payload %v", n)
	}
	if strings.Contains(string(plain), "review it") || strings.Contains(string(plain), "answered") {
		t.Fatalf("the payload carries content: %s", plain)
	}

	// The page on screen gets its alerts in place.
	h.do(http.MethodPost, "/api/presence", token, map[string]bool{"visible": true})
	h.do(http.MethodPost, "/api/send", token, map[string]string{"key": key, "text": "again"})
	p.until("the second alert", "alert", func(m map[string]any) bool { return m["kind"] == "finished" })
	select {
	case <-pushes:
		t.Fatal("pushed to a device whose page is on screen")
	case <-time.After(time.Second):
	}
}

// Revoking a device from the gateway's window closes its page's stream at
// once and refuses its credential.
func TestRevocationFromTheWindow(t *testing.T) {
	h := start(t)
	token, id := h.pair()
	p := h.open(token)
	p.until("the ready stream", "ready", nil)
	if out := h.cli("auth", "list"); !strings.Contains(out, id+"\tphone\t") {
		t.Fatalf("auth list: %s", out)
	}
	h.cli("auth", "revoke", id)
	select {
	case <-p.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked device's stream goes on")
	}
	if code, _ := h.do(http.MethodGet, "/api/devices", token, nil); code != http.StatusUnauthorized {
		t.Fatalf("a revoked credential: %d", code)
	}
}
