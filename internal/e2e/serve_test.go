package e2e

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

var serveAddress = regexp.MustCompile(`http://(127\.0\.0\.1:\d+)/v1/info`)

// The local HTTP contract uses the common gates with authenticated owner authority.
func TestServeCarriesOwnerVerbs(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_ANSWER": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")

	h.tmux("set-option", "-g", "@tmag-serve", "--origin *")
	if r := h.tmag("", "serve", "--open", "--server", h.name); r.code != 2 {
		t.Fatalf("invalid endpoint configuration accepted: %d", r.code)
	}
	h.tmux("set-option", "-g", "@tmag-serve", "--addr 127.0.0.1:0 --host box.example.ts.net")
	if r := h.tmag("", "serve", "--open", "--server", h.name); r.code != 0 {
		t.Fatalf("serve --open: %d %s", r.code, r.out)
	}
	windows := func() []string {
		var ids []string
		for _, line := range strings.Split(h.tmux("list-windows", "-a", "-F", "#{window_id} #{@tmag_serve}"), "\n") {
			if id, mark, _ := strings.Cut(line, " "); mark != "" {
				ids = append(ids, id)
			}
		}
		return ids
	}
	if len(windows()) != 1 {
		t.Fatalf("serve windows = %v", windows())
	}
	window := windows()[0]
	var addr string
	h.eventually("serve's address", func() bool {
		m := serveAddress.FindStringSubmatch(h.tmux("capture-pane", "-p", "-t", window))
		if m != nil {
			addr = m[1]
		}
		return addr != ""
	})
	store := auth.Store{Dir: os.Getenv("TMAG_AUTH_DIR")}
	issued, err := store.Issue(context.Background(), "e2e client", 0)
	if err != nil {
		t.Fatal(err)
	}
	token := issued.Token
	base := "http://" + addr
	h.eventually("API address in tmux", func() bool { return h.tmux("show-option", "-gqv", "@tmag_api_addr") == base })
	// Pressing the key again shows where it runs.
	if r := h.tmag("", "serve", "--open", "--server", h.name); r.code != 0 || len(windows()) != 1 {
		t.Fatalf("a second serve --open: %d %s, windows %v", r.code, r.out, windows())
	}

	client := &http.Client{Timeout: 30 * time.Second}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	// The stream carries the single local instance snapshot.
	watch, _ := http.NewRequest(http.MethodGet, base+"/v1/watch", nil)
	watch.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(watch)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				frames <- line
			}
		}
		close(frames)
	}()
	snapshot := ""
	for deadline := time.After(10 * time.Second); snapshot == ""; {
		select {
		case f := <-frames:
			if strings.Contains(f, `"type":"snapshot"`) {
				snapshot = f
			}
		case <-deadline:
			t.Fatal("no snapshot on the stream")
		}
	}
	if !strings.Contains(snapshot, `"type":"snapshot"`) || !strings.Contains(snapshot, sidA) {
		t.Fatalf("snapshot = %s", snapshot)
	}
	res.Body.Close()

	// An owner action records its admitted credential, with no agent identity.
	if code, body := call(http.MethodPost, "/v1/act", `{"verb":"send","target":"`+sidA+`","text":"from the phone"}`); code != http.StatusOK || !strings.Contains(body, `"via":"pane"`) {
		t.Fatalf("send: %d %s", code, body)
	}
	h.eventually("the agent receives the prompt", func() bool {
		got, _ := os.ReadFile(out)
		return strings.Contains(string(got), "from the phone")
	})
	h.waitSession(sidA, "idle")
	var sent eventlog.Event
	for _, e := range h.events(sidA) {
		if e.Event == eventlog.Send {
			sent = e
		}
	}
	if sent.Credential != issued.ID || sent.By != nil {
		t.Fatalf("Send event = %+v", sent)
	}
	h.eventually("the reply on the page", func() bool {
		code, body := call(http.MethodGet, "/v1/read?target="+sidA+"&last=1", "")
		return code == http.StatusOK && strings.Contains(body, `"text":"answered: from the phone`)
	})

	// A request: what it asks, then the answer.
	h.tmux("send-keys", "-t", pane, "block", "Enter")
	var req string
	h.eventually("a request", func() bool {
		s, ok := h.sessionOf(sidA)
		if ok && len(s.Requests) == 1 {
			req = s.Requests[0].ID
		}
		return req != ""
	})
	h.eventually("what the request asks", func() bool {
		code, body := call(http.MethodGet, "/v1/request?target="+sidA+"&request="+req, "")
		return code == http.StatusOK && strings.Contains(body, `"command":"date"`)
	})
	if code, body := call(http.MethodPost, "/v1/act", `{"verb":"answer","target":"`+sidA+`","request":"`+req+`","decision":"allow"}`); code != http.StatusOK {
		t.Fatalf("answer: %d %s", code, body)
	}
	h.eventually("the agent receives the answer", func() bool {
		got, _ := os.ReadFile(out)
		return strings.Contains(string(got), "hook:allow")
	})
	if code, body := call(http.MethodPost, "/v1/act", `{"verb":"answer","target":"`+sidA+`","request":"`+req+`","decision":"allow"}`); code != http.StatusConflict || !strings.Contains(body, "refused:changed") {
		t.Fatalf("answering twice: %d %s", code, body)
	}

	// A separate CLI process revokes admission. Existing gates still refused
	// the stale request above; authority does not bypass session state checks.
	if r := h.tmag("", "auth", "revoke", issued.ID); r.code != 0 {
		t.Fatalf("revoke: %d", r.code)
	}
	if code, _ := call(http.MethodGet, "/v1/list", ""); code != http.StatusUnauthorized {
		t.Fatalf("revoked client: %d", code)
	}
	// Closing the window stops it, and the page's address goes with it.
	h.tmux("kill-window", "-t", window)
	h.eventually("the page's address gone from tmux", func() bool {
		return h.tmux("show-option", "-gqv", "@tmag_api_addr") == ""
	})
}

func TestAgentCannotIssueOwnerCredentials(t *testing.T) {
	h := start(t)
	d := h.daemon()
	d.hook(map[string]any{"hook_event_name": "SessionStart", "session_id": sidC, "source": "startup"})
	h.waitSession(sidC, "idle")
	for _, verb := range []string{"issue", "pair"} {
		r := d.tool(sidC, "auth", verb, "agent-escape", "--json")
		if r.code != 3 || !strings.Contains(r.out, "refused:agent") {
			t.Fatalf("agent credential mutation %s: %d", verb, r.code)
		}
	}
	if r := h.tmag("", "auth", "issue", "person", "--json"); r.code != 0 {
		t.Fatalf("person issuance: %d", r.code)
	}
}
