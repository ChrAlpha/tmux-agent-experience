package gateway

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/push"
)

// fakeTmag writes a script that answers like tmag and records its arguments,
// one call per line, and what it read for send. watch is the body of its
// watch branch.
func fakeTmag(t *testing.T, watch string) (path, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + calls + `
case "$1" in
watch)
` + watch + `
;;
send)
	printf 'stdin=%s\n' "$(cat)" >> ` + calls + `
	echo '{"schema":"tmag/v2","via":"pane"}' ;;
answer)
	echo '{"schema":"tmag/v2","error":{"code":"refused:changed","message":"answered already"}}'
	exit 3 ;;
read)
	echo 'ssh: connect to host box: Connection refused' >&2
	exit 255 ;;
interrupt)
	sleep 1
	echo 'interrupt done' >> ` + calls + `
	echo '{"schema":"tmag/v2","via":"pane"}' ;;
*)
	echo '{"schema":"tmag/v2","ok":true}' ;;
esac
`
	path = filepath.Join(dir, "tmag")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func newServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	if cfg.Log == nil {
		cfg.Log = testLogger(t)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(func() {
		s.Close()
		hs.Close()
	})
	return s, hs
}

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(testWriter{t}, "", 0)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// signIn creates a valid credential directly in the gateway's store.
func signIn(t *testing.T, s *Server) auth.Generated {
	t.Helper()
	ctx := context.Background()
	p, err := s.auth.Pair(ctx, "test device", 0)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := s.auth.Redeem(ctx, p.Code)
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func call(t *testing.T, method, url, token, body string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set(authorizationHeader, "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decodeBody(t *testing.T, res *http.Response, v any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

type sse struct {
	event string
	data  string
}

func watch(t *testing.T, hs *httptest.Server, token string) <-chan sse {
	t.Helper()
	res := call(t, http.MethodGet, hs.URL+"/api/watch", token, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watch: %s", res.Status)
	}
	ch := make(chan sse, 32)
	go func() {
		defer close(ch)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		var cur sse
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if cur.data != "" {
					ch <- cur
				}
				cur = sse{}
				continue
			}
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				cur.event = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				cur.data = strings.TrimPrefix(v, " ")
			}
		}
	}()
	return ch
}

func waitFor(t *testing.T, ch <-chan sse, contains string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended waiting for %q", contains)
			}
			if f.event == "doc" && strings.Contains(f.data, contains) {
				return
			}
		case <-deadline:
			t.Fatalf("no doc event containing %q", contains)
		}
	}
}

func waitSeen(t *testing.T, ch <-chan sse, contains string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended waiting for a seen event containing %q", contains)
			}
			if f.event == "seen" && strings.Contains(f.data, contains) {
				return
			}
		case <-deadline:
			t.Fatalf("no seen event containing %q", contains)
		}
	}
}

func waitStreamClosed(t *testing.T, ch <-chan sse) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream stayed open")
		}
	}
}

func waitSession(t *testing.T, s *Server) Session {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if session, ok := s.states["here"].session("claude", "s1"); ok {
			return session
		}
		if time.Now().After(deadline) {
			t.Fatal("session here/claude/s1 never appeared")
		}
	}
}

func TestGuard(t *testing.T) {
	g := guard{owner: "me@example.com", tailscale: []string{"box.tail1234.ts.net"}}
	req := func(host string, header map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/machines", nil)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, c := range []struct {
		name   string
		r      *http.Request
		enters bool
	}{
		{"local", req("127.0.0.1:7777", nil), true},
		{"local by name", req("localhost:7777", nil), true},
		{"local over IPv6", req("[::1]:7777", nil), true},
		{"forwarded under a loopback name", req("127.0.0.1:7777", map[string]string{"X-Forwarded-For": "100.64.0.9"}), false},
		{"the owner through Tailscale", req("box.tail1234.ts.net", map[string]string{"Tailscale-User-Login": "Me@Example.com"}), true},
		{"someone else through Tailscale", req("box.tail1234.ts.net", map[string]string{"Tailscale-User-Login": "eve@example.com"}), false},
		{"a tagged device through Tailscale", req("box.tail1234.ts.net", nil), false},
		{"an unknown host name", req("evil.example", map[string]string{"Tailscale-User-Login": "me@example.com"}), false},
	} {
		if err := g.entry(c.r); (err == nil) != c.enters {
			t.Errorf("%s: entry = %v", c.name, err)
		}
	}
}

// signer issues Access tokens and publishes its keys, as a team would.
type signer struct {
	key     *rsa.PrivateKey
	kid     string
	fetches atomic.Int32
	srv     *httptest.Server
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &signer{key: key, kid: "k1"}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.fetches.Add(1)
		e := big.NewInt(int64(s.key.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": s.kid, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(s.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *signer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	part := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signed := part(map[string]string{"alg": "RS256", "kid": s.kid, "typ": "JWT"}) + "." + part(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestAccessTokens(t *testing.T) {
	s := newSigner(t)
	k := newAccessKeys(Access{Team: "myteam", Audience: "aud-tag"})
	k.certs = s.srv.URL
	now := time.Now()
	valid := func() map[string]any {
		return map[string]any{"aud": []string{"aud-tag"}, "iss": "https://myteam.cloudflareaccess.com", "email": "me@example.com", "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix()}
	}
	if email, err := k.verify(context.Background(), s.token(t, valid())); err != nil || email != "me@example.com" {
		t.Fatalf("valid token: %q %v", email, err)
	}
	one := valid()
	one["aud"] = "aud-tag"
	if _, err := k.verify(context.Background(), s.token(t, one)); err != nil {
		t.Fatalf("aud as a string: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"another application": func(c map[string]any) { c["aud"] = []string{"other"} },
		"another team":        func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" },
		"expired":             func(c map[string]any) { c["exp"] = now.Add(-2 * time.Hour).Unix() },
		"not valid yet":       func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() },
		"no email":            func(c map[string]any) { delete(c, "email") },
	} {
		c := valid()
		change(c)
		if _, err := k.verify(context.Background(), s.token(t, c)); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	tok := strings.Split(s.token(t, valid()), ".")
	forged, _ := json.Marshal(map[string]any{"aud": []string{"aud-tag"}, "iss": "https://myteam.cloudflareaccess.com", "email": "eve@example.com", "exp": now.Add(time.Hour).Unix()})
	tok[1] = base64.RawURLEncoding.EncodeToString(forged)
	if _, err := k.verify(context.Background(), strings.Join(tok, ".")); err == nil {
		t.Error("a forged token verified")
	}
	if _, err := k.verify(context.Background(), "not.a.token"); err == nil {
		t.Error("garbage verified")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s.key, s.kid = key, "k2"
	before := s.fetches.Load()
	k.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := k.verify(context.Background(), s.token(t, valid())); err != nil {
		t.Fatalf("after a rotation: %v", err)
	}
	s.kid = "k3"
	if _, err := k.verify(context.Background(), s.token(t, valid())); err == nil {
		t.Error("an unpublished key verified")
	}
	if got := s.fetches.Load() - before; got != 1 {
		t.Errorf("fetched the keys %d times for two unknown keys within a minute", got)
	}
}

func TestPageAndCredential(t *testing.T) {
	bin, _ := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, _ := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}, Owner: "me@example.com", Tailscale: []string{"box.tail1234.ts.net"}})
	hs := httptest.NewServer(s)
	defer hs.Close()
	// The page holds no data, and asks for a credential or a code.
	res := call(t, http.MethodGet, hs.URL+"/", "", "", nil)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "/app.js") || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("page: %s %s", res.Status, body)
	}
	if res := call(t, http.MethodGet, hs.URL+"/api/machines", "", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a credential: %s", res.Status)
	}
	// A tunnel's proof without a device credential opens nothing.
	tailscale := map[string]string{"Host": "box.tail1234.ts.net", "Tailscale-User-Login": "me@example.com"}
	if res := call(t, http.MethodGet, hs.URL+"/api/machines", "", "", tailscale); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Tailscale's headers without a credential: %s", res.Status)
	}
	// Pairing on this machine takes an invitation code.
	invitation, err := s.auth.Pair(context.Background(), "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	code := invitation.Code
	if res := call(t, http.MethodPost, hs.URL+"/api/pair", "", `{"code":"0000-0000-0000"}`, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a wrong code: %s", res.Status)
	}
	var paired struct {
		Credential auth.Generated `json:"credential"`
	}
	res = call(t, http.MethodPost, hs.URL+"/api/pair", "", `{"code":"`+code+`"}`, nil)
	decodeBody(t, res, &paired)
	if res.StatusCode != http.StatusOK || paired.Credential.Token == "" {
		t.Fatalf("the right code: %s %+v", res.Status, paired)
	}
	if res := call(t, http.MethodPost, hs.URL+"/api/pair", "", `{"code":"`+code+`"}`, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a code paired a second device: %s", res.Status)
	}
	if res := call(t, http.MethodGet, hs.URL+"/api/machines", paired.Credential.Token, "", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("with the paired credential: %s", res.Status)
	}
	// A tunnel's check that fails is not the credential's: the page keeps it.
	if res := call(t, http.MethodGet, hs.URL+"/api/machines", paired.Credential.Token, "", map[string]string{"Host": "box.tail1234.ts.net"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a credential without the tunnel's login: %s", res.Status)
	}
	res = call(t, http.MethodGet, hs.URL+"/icon.png", "", "", nil)
	img, err := png.Decode(res.Body)
	res.Body.Close()
	if err != nil || img.Bounds().Dx() != 180 {
		t.Fatalf("icon: %v", err)
	}
}

func TestWatchResumesAndFansOut(t *testing.T) {
	signal := filepath.Join(t.TempDir(), "signal")
	bin, calls := fakeTmag(t, `
echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[{"id":"s1","agent":"claude","state":"idle","turn":"t1","capabilities":["send"]}],"panes":[]}'
[ -n "$WATCH_SIGNAL" ] && while [ ! -f "$WATCH_SIGNAL" ]; do sleep 0.02; done
echo '{"schema":"tmag/v2","type":"event","event":{"seq":2,"time":"2026-11-12T08:00:00Z","agent":"claude","session":"s1","event":"Stop","state":"idle","turn":"t2"}}'
sleep 30`)
	t.Setenv("WATCH_SIGNAL", signal)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	waitSession(t, s)
	issued := signIn(t, s)
	one := watch(t, hs, issued.Token)
	two := watch(t, hs, issued.Token)
	waitFor(t, one, `"type":"snapshot"`)
	waitFor(t, two, `"type":"snapshot"`)
	if err := os.WriteFile(signal, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, one, `"seq":2`)
	waitFor(t, two, `"seq":2`)
	// Nothing resumes while the stream lives.
	b, _ := os.ReadFile(calls)
	if strings.Count(string(b), "watch") != 1 {
		t.Fatalf("watch calls = %q", b)
	}
}

func TestWatchResumesFromTheLastEvent(t *testing.T) {
	bin, calls := fakeTmag(t, `
echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'
echo '{"schema":"tmag/v2","type":"event","event":{"seq":2,"time":"2026-11-12T08:00:00Z","agent":"claude","session":"s1","event":"Stop","state":"idle"}}'
exit 1`)
	t.Setenv("WATCH_EXIT", "1")
	_, _ = newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(calls)
		runs := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(runs) >= 2 {
			if runs[0] != "watch" || runs[1] != "watch --since 2" {
				t.Fatalf("watch runs = %q", runs)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watch did not resume: %q", b)
		}
	}
}

func TestRevocationClosesStreamsAndDropsPush(t *testing.T) {
	bin, _ := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	issued := signIn(t, s)
	if err := s.auth.Subscribe(context.Background(), issued.ID, auth.Subscription{Endpoint: "https://push.example.com/x", P256DH: "a", Auth: "b"}); err != nil {
		t.Fatal(err)
	}
	one := watch(t, hs, issued.Token)
	waitFor(t, one, `"type":"snapshot"`)
	if err := s.auth.Revoke(context.Background(), issued.ID); err != nil {
		t.Fatal(err)
	}
	waitStreamClosed(t, one)
	_, list, err := s.auth.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("revoked credential remains: %+v %v", list, err)
	}
}

func TestSeenTurns(t *testing.T) {
	bin, _ := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[{"id":"s1","agent":"claude","state":"idle","turn":"t1","capabilities":["send"]},{"id":"s2","agent":"claude","state":"working","turn":"t2","capabilities":["send"]}],"panes":[]}'; sleep 30`)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	waitSession(t, s)
	issued := signIn(t, s)
	var machines struct {
		Seen map[string]string `json:"seen"`
	}
	res := call(t, http.MethodGet, hs.URL+"/api/machines", issued.Token, "", nil)
	decodeBody(t, res, &machines)
	if len(machines.Seen) != 0 {
		t.Fatalf("seen before opening: %v", machines.Seen)
	}
	var marked struct{ Turn string }
	// A working turn is not seen, and a stale turn is not this session's.
	for _, body := range []string{
		`{"machine":"here","agent":"claude","session":"s2","turn":"t2"}`,
		`{"machine":"here","agent":"claude","session":"s1","turn":"t0"}`,
	} {
		res = call(t, http.MethodPost, hs.URL+"/api/seen", issued.Token, body, nil)
		decodeBody(t, res, &marked)
		if marked.Turn != "" {
			t.Fatalf("marked a turn it should not: %s", body)
		}
	}
	res = call(t, http.MethodPost, hs.URL+"/api/seen", issued.Token, `{"machine":"here","agent":"claude","session":"s1","turn":"t1"}`, nil)
	decodeBody(t, res, &marked)
	if marked.Turn != "t1" {
		t.Fatalf("marked %q", marked.Turn)
	}
	res = call(t, http.MethodGet, hs.URL+"/api/machines", issued.Token, "", nil)
	decodeBody(t, res, &machines)
	if machines.Seen["here/claude/s1"] != "t1" {
		t.Fatalf("seen after opening: %v", machines.Seen)
	}
	// Every stream starts with the whole seen state.
	one := watch(t, hs, issued.Token)
	two := watch(t, hs, issued.Token)
	waitFor(t, one, `"seen":{"here/claude/s1":"t1"}`)
	waitFor(t, two, `"seen":{"here/claude/s1":"t1"}`)
	// A mark reaches every open page.
	res = call(t, http.MethodPost, hs.URL+"/api/seen", issued.Token, `{"machine":"here","agent":"claude","session":"s1","turn":"t1"}`, nil)
	decodeBody(t, res, &marked)
	waitSeen(t, one, `"session":"s1","turn":"t1"`)
	waitSeen(t, two, `"session":"s1","turn":"t1"`)
}

func TestAlertFiltering(t *testing.T) {
	idle := Session{State: "idle", Turn: "t1"}
	blocked := Session{ID: "s1", Agent: "claude", State: "blocked", Cwd: "/work/repo", Requests: []Request{{ID: "r1", Kind: "permission", Tool: "Bash"}}}
	for name, c := range map[string]struct {
		alert Alert
		s     Session
		holds bool
	}{
		"finished on its turn":       {Alert{Kind: "finished", Turn: "t1"}, idle, true},
		"finished on another turn":   {Alert{Kind: "finished", Turn: "t2"}, idle, false},
		"finished while working":     {Alert{Kind: "finished", Turn: "t1"}, Session{State: "working", Turn: "t1"}, false},
		"blocked on an open request": {Alert{Kind: "blocked", Request: &AlertRequest{ID: "r1"}}, blocked, true},
		"blocked after the answer":   {Alert{Kind: "blocked", Request: &AlertRequest{ID: "r2"}}, blocked, false},
		"blocked without a request":  {Alert{Kind: "blocked"}, blocked, false},
	} {
		if got := alertHolds(c.alert, c.s); got != c.holds {
			t.Errorf("%s: holds = %v", name, got)
		}
	}
	msg := alertMessage("box", blocked, Alert{Kind: "blocked", Request: &AlertRequest{ID: "r1", Kind: "permission", Tool: "Bash"}})
	if msg.Title != "claude needs your approval" || msg.Body != "Bash · repo · box" || msg.Machine != "box" || msg.Request != "r1" || msg.Urgency != "high" {
		t.Fatalf("blocked message: %+v", msg)
	}
	errSession := Session{State: "idle", Turn: "t1", Kind: "error", Agent: "codex", Cwd: "/work/repo"}
	msg = alertMessage("dev", errSession, Alert{Kind: "finished", Turn: "t1"})
	if msg.Title != "codex stopped on an error" || msg.Body != "repo · dev" || msg.Urgency != "normal" {
		t.Fatalf("finished message: %+v", msg)
	}
}

func TestAlertFiltersBeforeQueueing(t *testing.T) {
	state := newMachineState("box")
	state.apply(Event{Seq: 1, Agent: "claude", Session: "s1", State: "blocked", Requests: []Request{{ID: "r1", Kind: "permission", Tool: "Bash"}}})
	s := &Server{
		states: map[string]*machineState{"box": state},
		kinds:  map[string]bool{"blocked": true},
		queue:  make(chan alertJob, 1),
		push:   &push.Client{},
	}
	blocked := Event{Agent: "claude", Session: "s1", Alert: &Alert{Kind: "blocked", Present: false, Request: &AlertRequest{ID: "r1"}}}
	for name, e := range map[string]Event{
		"present":         {Agent: "claude", Session: "s1", Alert: &Alert{Kind: "blocked", Present: true, Request: &AlertRequest{ID: "r1"}}},
		"kind not chosen": {Agent: "claude", Session: "s1", Alert: &Alert{Kind: "finished", Present: false, Turn: "t1"}},
		"contradicted":    {Agent: "claude", Session: "s1", Alert: &Alert{Kind: "blocked", Present: false, Request: &AlertRequest{ID: "closed"}}},
		"unknown session": {Agent: "claude", Session: "gone", Alert: &Alert{Kind: "blocked", Present: false, Request: &AlertRequest{ID: "r1"}}},
	} {
		s.alert("box", e)
		if len(s.queue) != 0 {
			t.Fatalf("%s queued a push", name)
		}
	}
	s.alert("box", blocked)
	if len(s.queue) != 1 {
		t.Fatal("a deliverable alert was not queued")
	}
}

func TestVisibility(t *testing.T) {
	v := &visibility{at: map[string]time.Time{}}
	now := time.Now()
	v.set("a", true, now)
	v.set("b", true, now.Add(-2*time.Minute))
	if !v.visible("a", now.Add(30*time.Second)) {
		t.Fatal("a fresh report is not visible")
	}
	if v.visible("a", now.Add(2*time.Minute)) {
		t.Fatal("a report older than a minute still counts")
	}
	if v.visible("b", now) {
		t.Fatal("an old report counts")
	}
	v.keep(map[string]bool{})
	if v.visible("a", now) {
		t.Fatal("a forgotten device stays visible")
	}
}

func TestPushOnAlert(t *testing.T) {
	signal := filepath.Join(t.TempDir(), "signal")
	bin, _ := fakeTmag(t, `
echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[{"id":"s1","agent":"claude","state":"blocked","cwd":"/work/repo","turn":"t1","capabilities":["answer"],"requests":[{"id":"r1","kind":"permission","tool":"Bash","since":"2026-11-12T08:00:00Z"}]}],"panes":[]}'
[ -n "$WATCH_SIGNAL" ] && while [ ! -f "$WATCH_SIGNAL" ]; do sleep 0.02; done
echo '{"schema":"tmag/v2","type":"event","event":{"seq":2,"time":"2026-11-12T08:00:01Z","agent":"claude","session":"s1","event":"Alert","alert":{"kind":"blocked","present":false,"request":{"id":"r1","kind":"permission","tool":"Bash"}}}}'
sleep 30`)
	t.Setenv("WATCH_SIGNAL", signal)
	s, _ := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}, Owner: "me", Tailscale: []string{"box.ts.net"}, Push: []string{"blocked", "finished"}})
	waitSession(t, s)
	issued := signIn(t, s)
	got := make(chan *http.Request, 1)
	body := make(chan []byte, 1)
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body <- b
		got <- r
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()
	s.push.HTTP = pushSrv.Client()
	s.push.Endpoint = url.Parse
	if err := s.auth.Subscribe(context.Background(), issued.ID, auth.Subscription{
		Endpoint: pushSrv.URL + "/push", P256DH: testP256(t), Auth: testAuth(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signal, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") || r.Header.Get("Urgency") != "high" {
			t.Fatalf("push headers: %v", r.Header)
		}
		if len(<-body) == 0 {
			t.Fatal("empty push body")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push reached the service")
	}
}

func TestVisiblePageSkipsPush(t *testing.T) {
	signal := filepath.Join(t.TempDir(), "signal")
	bin, _ := fakeTmag(t, `
echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[{"id":"s1","agent":"claude","state":"blocked","cwd":"/work/repo","turn":"t1","capabilities":["answer"],"requests":[{"id":"r1","kind":"permission","tool":"Bash","since":"2026-11-12T08:00:00Z"}]}],"panes":[]}'
[ -n "$WATCH_SIGNAL" ] && while [ ! -f "$WATCH_SIGNAL" ]; do sleep 0.02; done
echo '{"schema":"tmag/v2","type":"event","event":{"seq":2,"time":"2026-11-12T08:00:01Z","agent":"claude","session":"s1","event":"Alert","alert":{"kind":"blocked","present":false,"request":{"id":"r1","kind":"permission","tool":"Bash"}}}}'
sleep 30`)
	t.Setenv("WATCH_SIGNAL", signal)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}, Owner: "me", Tailscale: []string{"box.ts.net"}, Push: []string{"blocked"}})
	waitSession(t, s)
	issued := signIn(t, s)
	got := make(chan struct{}, 1)
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()
	s.push.HTTP = pushSrv.Client()
	s.push.Endpoint = url.Parse
	if err := s.auth.Subscribe(context.Background(), issued.ID, auth.Subscription{
		Endpoint: pushSrv.URL + "/push", P256DH: testP256(t), Auth: testAuth(),
	}); err != nil {
		t.Fatal(err)
	}
	if res := call(t, http.MethodPost, hs.URL+"/api/visible", issued.Token, `{"visible":true}`, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("visibility report: %s", res.Status)
	}
	if err := os.WriteFile(signal, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
		t.Fatal("a visible page got a push")
	case <-time.After(time.Second):
	}
}

func TestGonePushDropsSubscription(t *testing.T) {
	bin, _ := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, _ := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}, Owner: "me", Tailscale: []string{"box.ts.net"}, Push: []string{"blocked"}})
	issued := signIn(t, s)
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer pushSrv.Close()
	s.push.HTTP = pushSrv.Client()
	s.push.Endpoint = url.Parse
	if err := s.auth.Subscribe(context.Background(), issued.ID, auth.Subscription{
		Endpoint: pushSrv.URL + "/push", P256DH: testP256(t), Auth: testAuth(),
	}); err != nil {
		t.Fatal(err)
	}
	s.states["here"].replace(1, []Session{{ID: "s1", Agent: "claude", State: "idle", Turn: "t1"}}, nil)
	s.sendDevice(alertJob{machine: "here", agent: "claude", session: "s1", alert: Alert{Kind: "finished", Turn: "t1"}, msg: push.Message{Kind: "finished"}}, issued.ID)
	_, list, err := s.auth.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Push {
		t.Fatalf("a gone subscription was kept: %+v %v", list, err)
	}
}

// testP256 is an uncompressed P-256 point for a fake subscription.
func testP256(t *testing.T) string {
	t.Helper()
	k, err := ecdhP256()
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(k)
}

func testAuth() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestPushValidation(t *testing.T) {
	bin, _ := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	issued := signIn(t, s)
	for _, endpoint := range []string{"http://push.example.com/x", "https://127.0.0.1/x", "https://10.0.0.1/x"} {
		res := call(t, http.MethodPost, hs.URL+"/api/push", issued.Token, `{"endpoint":"`+endpoint+`","keys":{"p256dh":"`+testP256(t)+`","auth":"`+testAuth()+`"}}`, nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s accepted: %s", endpoint, res.Status)
		}
	}
	res := call(t, http.MethodPost, hs.URL+"/api/push", issued.Token, `{"endpoint":"https://push.example.com/x","keys":{"p256dh":"`+testP256(t)+`","auth":"`+testAuth()+`"}}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a public endpoint: %s", res.Status)
	}
	_, list, err := s.auth.List(context.Background())
	if err != nil || len(list) != 1 || !list[0].Push {
		t.Fatalf("subscription not stored: %+v %v", list, err)
	}
	res = call(t, http.MethodDelete, hs.URL+"/api/push", issued.Token, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unsubscribe: %s", res.Status)
	}
	_, list, _ = s.auth.List(context.Background())
	if len(list) != 1 || list[0].Push {
		t.Fatalf("subscription not dropped: %+v", list)
	}
}

func TestActionsAreOneGatedVerbEach(t *testing.T) {
	bin, calls := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	issued := signIn(t, s)
	post := func(body string, header map[string]string) *http.Response {
		return call(t, http.MethodPost, hs.URL+"/api/act", issued.Token, body, header)
	}
	if res := post(`{"machine":"here","verb":"send","target":"--json","text":"review the diff"}`, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("send: %s", res.Status)
	}
	res := post(`{"machine":"here","verb":"answer","target":"s1","request":"r1","decision":"deny","message":"--not now"}`, nil)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "refused:changed") {
		t.Fatalf("a refused answer: %s %s", res.Status, body)
	}
	b, _ := os.ReadFile(calls)
	if got, want := string(b), "watch\nsend --json -- --json -\nstdin=review the diff\nanswer --json --message=--not now -- s1 r1 deny\n"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for name, header := range map[string]map[string]string{
		"as a form":         {"Content-Type": "application/x-www-form-urlencoded"},
		"from another site": {"Origin": "https://evil.example"},
	} {
		if res := post(`{"machine":"here","verb":"interrupt","target":"s1"}`, header); res.StatusCode == http.StatusOK {
			t.Errorf("an action %s went through", name)
		}
	}
	if res := call(t, http.MethodPost, hs.URL+"/api/act", "", `{"machine":"here","verb":"interrupt","target":"s1"}`, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an action without a credential: %s", res.Status)
	}
	if res := post(`{"machine":"elsewhere","verb":"interrupt","target":"s1"}`, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown machine: %s", res.Status)
	}
	res = call(t, http.MethodGet, hs.URL+"/api/read?machine=here&target=s1&last=1", issued.Token, "", nil)
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	var doc struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if res.StatusCode != http.StatusUnprocessableEntity || json.Unmarshal(body, &doc) != nil || !strings.Contains(doc.Error.Message, "Connection refused") {
		t.Fatalf("an unreachable machine: %s %s", res.Status, body)
	}
}

func TestActionsOutliveTheirRequest(t *testing.T) {
	bin, calls := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	issued := signIn(t, s)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/act", strings.NewReader(`{"machine":"here","verb":"interrupt","target":"s1"}`))
	req.Header.Set(authorizationHeader, "Bearer "+issued.Token)
	req.Header.Set("Content-Type", "application/json")
	if _, err := (&http.Client{Timeout: 200 * time.Millisecond}).Do(req); err == nil {
		t.Fatal("the request did not time out")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(calls); strings.Contains(string(b), "interrupt done") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the interrupt was cut off with the connection")
		}
	}
}

func TestMachineCommandsEndWhole(t *testing.T) {
	dir := t.TempDir()
	wrapper, pid := filepath.Join(dir, "wrapper"), filepath.Join(dir, "pid")
	script := "#!/bin/sh\nsleep 30 &\necho $! > " + pid + "\nwait\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := Machine{Name: "w", Argv: []string{wrapper}}.command(ctx, "watch")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for deadline := time.Now().Add(5 * time.Second); child == 0; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pid); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the wrapper started nothing")
		}
	}
	cancel()
	_ = cmd.Wait()
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(child, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the wrapper's child %d outlived the command", child)
		}
	}
}

func TestMachines(t *testing.T) {
	m, err := ParseMachine("box=ssh -p 2222 box tmag")
	if err != nil || m.Name != "box" || strings.Join(m.Argv, " ") != "ssh -p 2222 box tmag" {
		t.Fatalf("parsed %+v %v", m, err)
	}
	for _, bad := range []string{"box", "=ssh box", "box=", "b x=ssh box tmag"} {
		if _, err := ParseMachine(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	for _, spec := range []string{"box=ssh -p 2222 box tmag", "box=tsh ssh box tmag", "box=autossh -M 0 box tmag"} {
		m, _ := ParseMachine(spec)
		cmd := m.command(context.Background(), "send", "--", "it's", "-")
		if got := strings.Join(cmd.Args[len(cmd.Args)-4:], " "); got != `'send' '--' 'it'\''s' '-'` {
			t.Fatalf("%s: arguments = %s", spec, got)
		}
	}
	m, _ = ParseMachine("dev=docker exec -i dev tmag")
	if cmd := m.command(context.Background(), "it's"); cmd.Args[len(cmd.Args)-1] != "it's" {
		t.Fatalf("docker exec arguments = %q", cmd.Args)
	}
	for _, bad := range []Config{
		{Machines: []Machine{{Name: "a", Argv: []string{"tmag"}}}},
		{Machines: []Machine{{Name: "a", Argv: []string{"tmag"}}, {Name: "a", Argv: []string{"tmag"}}}, StateDir: t.TempDir()},
		{Machines: []Machine{{Name: "a", Argv: []string{"tmag"}}}, StateDir: t.TempDir(), Tailscale: []string{"box.ts.net"}},
	} {
		if _, err := New(bad); err == nil {
			t.Errorf("config %+v accepted", bad)
		}
	}
	if _, err := New(Config{Machines: []Machine{{Name: "a", Argv: []string{"tmag"}}}, StateDir: t.TempDir(), Access: Access{Hosts: []string{"t.example.com"}}, Owner: "me"}); err == nil {
		t.Error("Access without a team and an audience tag accepted")
	}
}

func TestHTTPWorkerArgumentsAreNotShellQuoted(t *testing.T) {
	m, err := ParseMachine("remote=tmag")
	if err != nil {
		t.Fatal(err)
	}
	m.Argv = []string{"tmag-client", "http", "https://ssh", "/private/autossh"}
	cmd := m.command(context.Background(), "watch", "--since", "17")
	if got := strings.Join(cmd.Args[len(cmd.Args)-3:], " "); got != "watch --since 17" {
		t.Fatalf("HTTP worker arguments were quoted: %s", got)
	}
}

func ecdhP256() ([]byte, error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return key.PublicKey().Bytes(), nil
}
