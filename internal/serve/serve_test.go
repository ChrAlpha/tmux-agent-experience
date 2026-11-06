package serve

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const token = "0123456789abcdef0123456789abcdef"

// fakeTmag writes a script that answers like tmag and records its
// arguments, one call per line, and what it read for send.
func fakeTmag(t *testing.T) (path, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + calls + `
case "$1" in
watch)
	echo '{"schema":"tmag/v1","type":"snapshot","seq":5,"sessions":[],"panes":[]}'
	echo 'not a document'
	echo '{"schema":"tmag/v1","type":"event","event":{"seq":6,"agent":"claude","session":"s1","event":"Stop","state":"idle"}}'
	[ -n "$WATCH_EXIT" ] && exit 1
	sleep 30 ;;
send)
	printf 'stdin=%s\n' "$(cat)" >> ` + calls + `
	echo '{"schema":"tmag/v1","via":"pane"}' ;;
answer)
	echo '{"schema":"tmag/v1","error":{"code":"refused:changed","message":"answered already"}}'
	exit 3 ;;
read)
	echo 'ssh: connect to host box: Connection refused' >&2
	exit 255 ;;
interrupt)
	sleep 1
	echo 'interrupt done' >> ` + calls + `
	echo '{"schema":"tmag/v1","via":"pane"}' ;;
*)
	echo '{"schema":"tmag/v1","ok":true}' ;;
esac
`
	path = filepath.Join(dir, "tmag")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func newServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Token == "" {
		cfg.Token = token
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return hs
}

func get(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: token})
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGuard(t *testing.T) {
	g := guard{owner: "me@example.com", tailscale: []string{"box.tail1234.ts.net"}, token: token}
	req := func(host string, header map[string]string, cookie bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/machines", nil)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		if cookie {
			r.AddCookie(&http.Cookie{Name: tokenCookie, Value: token})
		}
		return r
	}
	for _, c := range []struct {
		name   string
		r      *http.Request
		admits bool
	}{
		{"local with the token", req("127.0.0.1:7777", nil, true), true},
		{"local by name, with the token", req("localhost:7777", nil, true), true},
		{"local without the token", req("127.0.0.1:7777", nil, false), false},
		{"local with the token in the URL", httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7777/?token="+token, nil), true},
		{"local with a wrong token", httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7777/?token=nope", nil), false},
		{"forwarded under a loopback name", req("127.0.0.1:7777", map[string]string{"X-Forwarded-For": "100.64.0.9"}, true), false},
		{"the owner through Tailscale", req("box.tail1234.ts.net", map[string]string{"Tailscale-User-Login": "Me@Example.com"}, false), true},
		{"someone else through Tailscale", req("box.tail1234.ts.net", map[string]string{"Tailscale-User-Login": "eve@example.com"}, false), false},
		{"a tagged device through Tailscale", req("box.tail1234.ts.net", nil, false), false},
		{"an unknown host name", req("evil.example", map[string]string{"Tailscale-User-Login": "me@example.com"}, true), false},
	} {
		if err := g.admit(c.r); (err == nil) != c.admits {
			t.Errorf("%s: admit = %v", c.name, err)
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
	one["aud"] = "aud-tag" // a single audience as a string
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
	// A token whose claims were changed after signing.
	tok := strings.Split(s.token(t, valid()), ".")
	forged, _ := json.Marshal(map[string]any{"aud": []string{"aud-tag"}, "iss": "https://myteam.cloudflareaccess.com", "email": "eve@example.com", "exp": now.Add(time.Hour).Unix()})
	tok[1] = base64.RawURLEncoding.EncodeToString(forged)
	if _, err := k.verify(context.Background(), strings.Join(tok, ".")); err == nil {
		t.Error("a forged token verified")
	}
	if _, err := k.verify(context.Background(), "not.a.token"); err == nil {
		t.Error("garbage verified")
	}

	// Access rotates its key: an unknown key id fetches the keys again, at
	// most once a minute.
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

func TestAccessThroughTheGuard(t *testing.T) {
	s := newSigner(t)
	g := guard{owner: "me@example.com", access: []string{"tmag.example.com"}, token: token, keys: newAccessKeys(Access{Team: "myteam", Audience: "aud-tag"})}
	g.keys.certs = s.srv.URL
	claims := map[string]any{"aud": "aud-tag", "iss": "https://myteam.cloudflareaccess.com", "email": "me@example.com", "exp": time.Now().Add(time.Hour).Unix()}
	r := httptest.NewRequest(http.MethodGet, "http://tmag.example.com/", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", s.token(t, claims))
	if err := g.admit(r); err != nil {
		t.Fatalf("the owner through Access: %v", err)
	}
	// The plain email header proves nothing.
	r = httptest.NewRequest(http.MethodGet, "http://tmag.example.com/", nil)
	r.Header.Set("Cf-Access-Authenticated-User-Email", "me@example.com")
	if err := g.admit(r); err == nil {
		t.Fatal("admitted on the email header alone")
	}
}

func TestPageAndAPIAreForTheOwnerOnly(t *testing.T) {
	bin, _ := fakeTmag(t)
	hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	res, err := http.Get(hs.URL + "/api/machines")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("without the token: %s", res.Status)
	}
	// The token in the URL becomes a cookie, out of the address bar.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = client.Get(hs.URL + "/?token=" + token)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || !strings.Contains(res.Header.Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("token in the URL: %s %v", res.Status, res.Header)
	}
	res = get(t, hs.URL+"/", nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "/app.js") || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("page: %s %s", res.Status, body)
	}
	// What a browser fetches without credentials holds nothing private.
	res, _ = http.Get(hs.URL + "/icon.png")
	img, err := png.Decode(res.Body)
	if err != nil || img.Bounds().Dx() != 180 {
		t.Fatalf("icon: %v", err)
	}
}

func TestWatchRelaysEachMachineAndResumes(t *testing.T) {
	bin, calls := fakeTmag(t)
	t.Setenv("WATCH_EXIT", "1")
	hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	res := get(t, hs.URL+"/api/watch?since=here:3", nil)
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", res.Header.Get("Content-Type"))
	}
	frames := bufio.NewScanner(res.Body)
	var got []string
	for frames.Scan() && len(got) < 7 {
		if line := frames.Text(); strings.HasPrefix(line, "data: ") {
			got = append(got, strings.TrimPrefix(line, "data: "))
		}
	}
	want := []string{
		`{"error":"","machine":"here","up":true}`,
		`{"machine":"here","doc":{"schema":"tmag/v1","type":"snapshot","seq":5,"sessions":[],"panes":[]}}`,
		`{"machine":"here","doc":{"schema":"tmag/v1","type":"event","event":{"seq":6,"agent":"claude","session":"s1","event":"Stop","state":"idle"}}}`,
	}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("frame %d = %q, want %q (all: %q)", i, got, w, got)
		}
	}
	if !strings.Contains(got[3], `"up":false`) {
		t.Fatalf("the end of a stream: %q", got[3])
	}
	b, _ := os.ReadFile(calls)
	runs := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(runs) < 2 || runs[0] != "watch --since 3" || runs[1] != "watch --since 6" {
		t.Fatalf("watch runs = %q, want the page's cursor, then the last relayed", runs)
	}
}

func TestActionsAreOneGatedVerbEach(t *testing.T) {
	bin, calls := fakeTmag(t)
	hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	post := func(body string, header map[string]string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/act", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: tokenCookie, Value: token})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tmag-Action", "1")
		for k, v := range header {
			if v == "" {
				req.Header.Del(k)
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
	if res := post(`{"machine":"here","verb":"send","target":"--json","text":"review the diff"}`, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("send: %s", res.Status)
	}
	res := post(`{"machine":"here","verb":"answer","target":"s1","request":"r1","decision":"deny","message":"--not now"}`, nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "refused:changed") {
		t.Fatalf("a refused answer: %s %s", res.Status, body)
	}
	b, _ := os.ReadFile(calls)
	if got, want := string(b), "send --json -- --json -\nstdin=review the diff\nanswer --json --message=--not now -- s1 r1 deny\n"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for name, header := range map[string]map[string]string{
		"without the header": {"X-Tmag-Action": ""},
		"as a form":          {"Content-Type": "application/x-www-form-urlencoded"},
		"from another site":  {"Origin": "https://evil.example"},
	} {
		if res := post(`{"machine":"here","verb":"interrupt","target":"s1"}`, header); res.StatusCode == http.StatusOK {
			t.Errorf("an action %s went through", name)
		}
	}
	if res := post(`{"machine":"elsewhere","verb":"interrupt","target":"s1"}`, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown machine: %s", res.Status)
	}
	// A machine out of reach answers with its own words, as tmag's error.
	res = get(t, hs.URL+"/api/read?machine=here&target=s1&last=1", nil)
	body, _ = io.ReadAll(res.Body)
	var doc struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if res.StatusCode != http.StatusUnprocessableEntity || json.Unmarshal(body, &doc) != nil || !strings.Contains(doc.Error.Message, "Connection refused") {
		t.Fatalf("an unreachable machine: %s %s", res.Status, body)
	}
}

// A verb runs to its end even when the phone that asked for it loses its
// connection meanwhile.
func TestActionsOutliveTheirRequest(t *testing.T) {
	bin, calls := fakeTmag(t)
	hs := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}})
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/act", strings.NewReader(`{"machine":"here","verb":"interrupt","target":"s1"}`))
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: token})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tmag-Action", "1")
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
	// Through ssh, every argument reaches tmag as it was.
	cmd := m.command(context.Background(), "send", "--", "it's", "-")
	if got := strings.Join(cmd.Args[len(cmd.Args)-4:], " "); got != `'send' '--' 'it'\''s' '-'` {
		t.Fatalf("ssh arguments = %s", got)
	}
	for _, bad := range []Config{{}, {Machines: []Machine{{Name: "a", Argv: []string{"tmag"}}}, Token: token, Tailscale: []string{"box.ts.net"}}} {
		if _, err := New(bad); err == nil {
			t.Errorf("config %+v accepted", bad)
		}
	}
	if _, err := New(Config{Machines: []Machine{{Name: "a", Argv: []string{"tmag"}}}, Token: token, Access: Access{Hosts: []string{"t.example.com"}}, Owner: "me"}); err == nil {
		t.Error("Access without a team and an audience tag accepted")
	}
}
