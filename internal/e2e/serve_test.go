package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

var (
	serveAddress = regexp.MustCompile(`http://(127\.0\.0\.1:\d+)/#token=([0-9a-f]+)`)
	pairingCode  = regexp.MustCompile(`enter (\d{6})`)
)

// tmag serve runs in a tmux window as prefix + M-r opens it, serves the page
// what each agent does, and carries the verbs as a person's.
func TestServeCarriesThePageVerbs(t *testing.T) {
	h := start(t)
	out := filepath.Join(h.dir, "received")
	pane := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidA, "FAKE_OUT": out, "FAKE_HOOKS": "1", "FAKE_ANSWER": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(pane, "idle", "process")

	h.tmux("set-option", "-g", "@tmag-serve", "--addr 127.0.0.1:0 --name here --tailscale box.example.ts.net --owner me@example.com")
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
	var addr, token, code string
	h.eventually("serve's address and pairing code in its window", func() bool {
		screen := h.tmux("capture-pane", "-p", "-t", window)
		m, c := serveAddress.FindStringSubmatch(screen), pairingCode.FindStringSubmatch(screen)
		if m != nil && c != nil {
			addr, token, code = m[1], m[2], c[1]
		}
		return code != ""
	})
	base := "http://" + addr
	h.eventually("the page's address in tmux", func() bool {
		return h.tmux("show-option", "-gqv", "@tmag_serve_url") == "https://box.example.ts.net"
	})
	// Pressing the key again shows where it runs.
	if r := h.tmag("", "serve", "--open", "--server", h.name); r.code != 0 || len(windows()) != 1 {
		t.Fatalf("a second serve --open: %d %s, windows %v", r.code, r.out, windows())
	}

	client := &http.Client{Timeout: 30 * time.Second}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("X-Tmag-Token", token)
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

	// The stream: the machine, then its snapshot, with the session in it.
	watch, _ := http.NewRequest(http.MethodGet, base+"/api/watch", nil)
	watch.Header.Set("X-Tmag-Token", token)
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
	if !strings.Contains(snapshot, `"machine":"here"`) || !strings.Contains(snapshot, sidA) {
		t.Fatalf("snapshot = %s", snapshot)
	}
	res.Body.Close()

	// A prompt from the page, as a person's: from the serve window's pane,
	// by no agent.
	if code, body := call(http.MethodPost, "/api/act", `{"machine":"here","verb":"send","target":"`+sidA+`","text":"from the phone"}`); code != http.StatusOK || !strings.Contains(body, `"via":"pane"`) {
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
	if sent.From == nil || sent.By != nil {
		t.Fatalf("Send event = %+v", sent)
	}
	h.eventually("the reply on the page", func() bool {
		code, body := call(http.MethodGet, "/api/read?machine=here&target="+sidA+"&last=1", "")
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
		code, body := call(http.MethodGet, "/api/request?machine=here&target="+sidA+"&request="+req, "")
		return code == http.StatusOK && strings.Contains(body, `"command":"date"`)
	})
	if code, body := call(http.MethodPost, "/api/act", `{"machine":"here","verb":"answer","target":"`+sidA+`","request":"`+req+`","decision":"allow"}`); code != http.StatusOK {
		t.Fatalf("answer: %d %s", code, body)
	}
	h.eventually("the agent receives the answer", func() bool {
		got, _ := os.ReadFile(out)
		return strings.Contains(string(got), "hook:allow")
	})
	if code, body := call(http.MethodPost, "/api/act", `{"machine":"here","verb":"answer","target":"`+sidA+`","request":"`+req+`","decision":"allow"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "refused:changed") {
		t.Fatalf("answering twice: %d %s", code, body)
	}

	// Through Tailscale: the owner's login and a paired device's token. The
	// login alone is what any process on this machine can claim.
	tailscale := func(method, path, login, tok, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Host = "box.example.ts.net"
		req.Header.Set("Tailscale-User-Login", login)
		if tok != "" {
			req.Header.Set("X-Tmag-Token", tok)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := tailscale(http.MethodGet, "/api/machines", "me@example.com", "", ""); code != http.StatusForbidden {
		t.Fatalf("the owner's login without a token: %d %s", code, body)
	}
	if code, body := tailscale(http.MethodGet, "/api/machines", "eve@example.com", token, ""); code != http.StatusForbidden {
		t.Fatalf("someone else's login: %d %s", code, body)
	}
	// A phone pairs with the code the window shows, and gets the token.
	status, body := tailscale(http.MethodPost, "/api/pair", "me@example.com", "", `{"code":"`+code+`"}`)
	var paired struct{ Token string }
	if status != http.StatusOK || json.Unmarshal([]byte(body), &paired) != nil || paired.Token != token {
		t.Fatalf("pairing: %d %s", status, body)
	}
	if code, body := tailscale(http.MethodGet, "/api/machines", "me@example.com", paired.Token, ""); code != http.StatusOK || !strings.Contains(body, `"here"`) {
		t.Fatalf("a paired phone through Tailscale: %d %s", code, body)
	}

	// Closing the window stops it, and the page's address goes with it.
	h.tmux("kill-window", "-t", window)
	h.eventually("the page's address gone from tmux", func() bool {
		return h.tmux("show-option", "-gqv", "@tmag_serve_url") == ""
	})
}
