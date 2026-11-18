package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

func fixture(t *testing.T, run Runner) (*Server, auth.Store, auth.Issued) {
	t.Helper()
	store := auth.Store{Dir: t.TempDir()}
	c, err := store.Issue(context.Background(), "test client", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Auth: store, Run: run, Hosts: []string{"box.example"}, Origins: []string{"https://client.example"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, store, c
}
func call(s *Server, method, path, body, token, host, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	if host != "" {
		r.Host = host
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestAdmissionAndIndependentContract(t *testing.T) {
	calls := 0
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		calls++
		if query.OwnerGrant(ctx).ID() == "" {
			t.Error("owner grant lost")
		}
		_, _ = io.WriteString(out, `{"schema":"tmag/v2","sessions":[]}`)
		return 0
	})
	for _, v := range []struct {
		token, host, origin string
		status              int
	}{
		{"", "localhost", "", 401}, {c.Token, "evil.example", "", 403}, {c.Token, "localhost", "https://evil.example", 403},
		{c.Token, "localhost", "https://localhost", 403},
		{c.Token, "box.example", "https://client.example", 200}, {c.Token, "localhost", "", 200},
	} {
		res := call(s, "GET", "/v2/list", "", v.token, v.host, v.origin)
		if res.Code != v.status {
			t.Errorf("admission = %d, want %d", res.Code, v.status)
		}
	}
	if calls != 2 {
		t.Fatalf("unauthorized request reached the runner (%d)", calls)
	}
	info := call(s, "GET", "/v2/info", "", c.Token, "", "")
	if !strings.Contains(info.Body.String(), Schema) || !strings.Contains(info.Body.String(), "instance") {
		t.Fatal("missing public contract discovery")
	}
	if !strings.Contains(info.Body.String(), `"send"`) {
		t.Fatal("send is not among the operations")
	}
	for _, path := range []string{"/app.js", "/", "/api/machines"} {
		if got := call(s, "GET", path, "", c.Token, "", ""); got.Code != 404 {
			t.Errorf("client route %s is still served", path)
		}
	}
	preflight := call(s, "OPTIONS", "/v2/act", "", "", "box.example", "https://client.example")
	if preflight.Code != 204 || preflight.Header().Get("Access-Control-Allow-Origin") != "https://client.example" {
		t.Fatal("explicit client origin cannot preflight")
	}
}
func TestActionsPreserveArgumentsAndGates(t *testing.T) {
	var got []string
	var text string
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		got = args
		if in != nil {
			b, _ := io.ReadAll(in)
			text = string(b)
		}
		_, _ = io.WriteString(out, `{"schema":"tmag/v2","error":{"code":"refused:changed","message":"changed"}}`)
		return 3
	})
	r := call(s, "POST", "/v2/act", `{"verb":"send","target":"--json","text":"hello"}`, c.Token, "", "")
	if r.Code != 409 || strings.Join(got, " ") != "send --json -- --json -" || text != "hello" {
		t.Fatal("control arguments or refusal changed")
	}
	r = call(s, "POST", "/v2/act", `{"verb":"shell","target":"--json","text":"hello"}`, c.Token, "", "")
	if r.Code != 400 {
		t.Fatalf("an unknown control verb: %d", r.Code)
	}
	got = nil
	for _, b := range []string{`{"verb":"shell","target":"a"}`, `{"verb":"send","target":"a","machine":"remote"}`, `{"verb":"send","target":"a"} {}`} {
		if res := call(s, "POST", "/v2/act", b, c.Token, "", ""); res.Code != 400 {
			t.Error("non-contract action accepted")
		}
	}
	if got != nil {
		t.Fatal("invalid action reached local CLI")
	}
}

// A read names one of what it reads, within its bounds, and the target stays
// literal.
func TestReadTakesOneOfLastLinesOrTurns(t *testing.T) {
	var got []string
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		got = args
		_, _ = io.WriteString(out, `{"schema":"tmag/v2"}`)
		return 0
	})
	for query, want := range map[string]string{
		"last=1":   "read --json --last -- --x",
		"lines=40": "read --json --lines 40 -- --x",
		"turns=20": "read --json --turns 20 -- --x",
		"turns=07": "read --json --turns 7 -- --x",
	} {
		got = nil
		if r := call(s, "GET", "/v2/read?target=--x&"+query, "", c.Token, "", ""); r.Code != 200 || strings.Join(got, " ") != want {
			t.Errorf("%s: %d %q", query, r.Code, got)
		}
	}
	for _, query := range []string{"", "last=1&turns=2", "lines=5&turns=2", "turns=0", "turns=21", "turns=x", "lines=2001"} {
		got = nil
		if r := call(s, "GET", "/v2/read?target=a&"+query, "", c.Token, "", ""); r.Code != 400 || got != nil {
			t.Errorf("%q: %d, ran %q", query, r.Code, got)
		}
	}
}
func TestPairingIssuesIndependentCredentials(t *testing.T) {
	s, store, c := fixture(t, func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { return 0 })
	p, err := store.Pair(context.Background(), "phone", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"code": p.Code})
	res := call(s, "POST", "/v2/pair", string(b), "", "box.example", "")
	var d struct{ Credential auth.Issued }
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &d) != nil || d.Credential.ID == c.ID {
		t.Fatal("pairing did not issue its own credential")
	}
	if _, err = store.Authenticate(context.Background(), d.Credential.Token); err != nil {
		t.Fatal(err)
	}
	if res = call(s, "POST", "/v2/pair", string(b), "", "box.example", ""); res.Code != 403 {
		t.Fatal("pairing code reused")
	}
}
func TestRevocationClosesAnExistingSubscription(t *testing.T) {
	stopped := make(chan struct{})
	s, store, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		defer close(stopped)
		if strings.Join(args, " ") != "watch --since 17" {
			t.Error("cursor not relayed")
		}
		_, _ = io.WriteString(out, "{\"schema\":\"tmag/v2\",\"type\":\"snapshot\",\"seq\":18}\n")
		<-ctx.Done()
		return 0
	})
	hs := httptest.NewServer(s)
	defer hs.Close()
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v2/watch?since=17", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(res.Body); done <- string(b) }()
	if err = store.Revoke(context.Background(), c.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-done:
		if !strings.Contains(data, "id: 18") {
			t.Fatal("missing cursor frame")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("revoked watch stayed open")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("local watch outlived subscription")
	}
}

// A watch relays each document of the local watch as one SSE frame, with
// the snapshot's or event's sequence number as its id, so that a client
// reconnects with Last-Event-ID; a replacement snapshot is one more frame.
func TestWatchRelaysSnapshotsAndEventsWithNumericIDs(t *testing.T) {
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		if strings.Join(args, " ") != "watch --since 17" {
			t.Errorf("runner args = %q", args)
		}
		_, _ = io.WriteString(out, "{\"schema\":\"tmag/v2\",\"type\":\"event\",\"event\":{\"seq\":18}}\n")
		_, _ = io.WriteString(out, "{\"schema\":\"tmag/v2\",\"type\":\"snapshot\",\"seq\":29,\"sessions\":[],\"panes\":[]}\n")
		return 0
	})
	res := call(s, "GET", "/v2/watch?since=17", "", c.Token, "box.example", "")
	data := res.Body.String()
	if res.Code != 200 || !strings.Contains(data, "id: 18\nevent: doc") || !strings.Contains(data, "id: 29\nevent: doc") || strings.Count(data, "id:") != 2 {
		t.Fatalf("watch frames: %d %s", res.Code, data)
	}
}
func TestAcceptedActionOutlivesDisconnect(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		close(started)
		<-release
		if ctx.Err() != nil {
			t.Error("accepted action canceled by disconnect")
		}
		_, _ = io.WriteString(out, `{"schema":"tmag/v2"}`)
		close(finished)
		return 0
	})
	hs := httptest.NewServer(s)
	defer hs.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hs.URL+"/v2/act", strings.NewReader(`{"verb":"interrupt","target":"session"}`))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
	}()
	<-started
	cancel()
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("action did not finish")
	}
}

type stalledBody struct {
	io.Reader
	started chan struct{}
	release chan struct{}
	first   bool
}

func (b *stalledBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		close(b.started)
		<-b.release
	}
	return b.Reader.Read(p)
}
func (*stalledBody) Close() error { return nil }
func TestRevocationBeforeBodyCompletionPreventsAdmission(t *testing.T) {
	ran := false
	s, store, c := fixture(t, func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { ran = true; return 0 })
	body := &stalledBody{Reader: strings.NewReader(`{"verb":"interrupt","target":"session"}`), started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/v2/act", body)
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	<-body.started
	if err := store.Revoke(context.Background(), c.ID, false); err != nil {
		t.Fatal(err)
	}
	close(body.release)
	<-done
	if ran || w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked upload executed: %v, status %d", ran, w.Code)
	}
}

// A start names an agent or a template, and an absolute directory: never a
// command, and the name stays literal.
func TestStartNamesAnAgentOrATemplate(t *testing.T) {
	var got []string
	var text string
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		got, text = args, ""
		if in != nil {
			b, _ := io.ReadAll(in)
			text = string(b)
		}
		_, _ = io.WriteString(out, `{"schema":"tmag/v2"}`)
		return 0
	})
	r := call(s, "POST", "/v2/act", `{"verb":"start","template":"review","dir":"/src/api","worktree":"fix/x","text":"look"}`, c.Token, "", "")
	if r.Code != 200 || strings.Join(got, " ") != "start --json --dir=/src/api --worktree=fix/x -- review -" || text != "look" {
		t.Fatalf("a template start: %d %q %q", r.Code, got, text)
	}
	r = call(s, "POST", "/v2/act", `{"verb":"start","agent":"codex","dir":"/src/api"}`, c.Token, "", "")
	if r.Code != 200 || strings.Join(got, " ") != "start --json --dir=/src/api -- codex" || text != "" {
		t.Fatalf("an agent start: %d %q", r.Code, got)
	}
	got = nil
	for _, b := range []string{
		`{"verb":"start","agent":"sh","dir":"/src"}`,
		`{"verb":"start","template":"claude","dir":"/src"}`,
		`{"verb":"start","template":"--version","dir":"/src"}`,
		`{"verb":"start","agent":"claude","template":"review","dir":"/src"}`,
		`{"verb":"start","agent":"claude","dir":"src"}`,
		`{"verb":"start","agent":"claude"}`,
		`{"verb":"start","agent":"claude","dir":"/src","target":"%1"}`,
		`{"verb":"start","agent":"claude","dir":"/src","command":"rm -rf /"}`,
	} {
		if res := call(s, "POST", "/v2/act", b, c.Token, "", ""); res.Code != 400 {
			t.Errorf("%s: %d", b, res.Code)
		}
	}
	if got != nil {
		t.Fatalf("an invalid start reached the local CLI: %q", got)
	}
}

// Keys name the screen they answer and the keys, which stay literal.
func TestKeysNameTheirScreen(t *testing.T) {
	var got []string
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		got = args
		_, _ = io.WriteString(out, `{"schema":"tmag/v2"}`)
		return 0
	})
	r := call(s, "POST", "/v2/act", `{"verb":"keys","target":"%3","screen":"abc","text":"yes","keys":["Enter","--json"]}`, c.Token, "", "")
	if r.Code != 200 || strings.Join(got, " ") != "keys --json --screen=abc --text=yes -- %3 Enter --json" {
		t.Fatalf("keys: %d %q", r.Code, got)
	}
	got = nil
	for _, b := range []string{`{"verb":"keys","target":"%3","keys":["Enter"]}`, `{"verb":"keys","screen":"abc","keys":["Enter"]}`} {
		if res := call(s, "POST", "/v2/act", b, c.Token, "", ""); res.Code != 400 {
			t.Errorf("%s: %d", b, res.Code)
		}
	}
	if got != nil {
		t.Fatalf("invalid keys reached the local CLI: %q", got)
	}
}

// A changes read names its target, and a file, literally.
func TestChangesNameTheirTarget(t *testing.T) {
	var got []string
	s, _, c := fixture(t, func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		got = args
		_, _ = io.WriteString(out, `{"schema":"tmag/v2"}`)
		return 0
	})
	if r := call(s, "GET", "/v2/changes?target=--json&file=a.go", "", c.Token, "", ""); r.Code != 200 || strings.Join(got, " ") != "changes --json --file=a.go -- --json" {
		t.Fatalf("changes: %d %q", r.Code, got)
	}
	if r := call(s, "GET", "/v2/changes", "", c.Token, "", ""); r.Code != 400 {
		t.Fatalf("changes without a target: %d", r.Code)
	}
}

// Allowed hosts and origins take what people paste: an origin keeps the form a
// browser sends, and a value holding more than a host or an origin is refused
// with the flag that says what it meant.
func TestAllowedHostsAndOriginsTakeWhatPeoplePaste(t *testing.T) {
	store := auth.Store{Dir: t.TempDir()}
	run := func(_ context.Context, _ []string, _ io.Reader, out, _ io.Writer) int {
		_, _ = io.WriteString(out, `{"schema":"tmag/v2","sessions":[]}`)
		return 0
	}
	for _, c := range []struct {
		hosts, origins []string
		err            string
	}{
		{hosts: []string{"https://Box.example.ts.net/"}, err: "--host box.example.ts.net"},
		{hosts: []string{"http://box.example:8080/v2/info"}, err: "--host box.example:8080"},
		{hosts: []string{"a b"}, err: `--host "a b" is not a host`},
		{origins: []string{"https://client.example/app?x=1"}, err: "--origin https://client.example"},
		{origins: []string{"https://me@client.example"}, err: "--origin https://client.example"},
		{origins: []string{"https://client.example//"}, err: "--origin https://client.example"},
		{origins: []string{"*"}, err: `--origin "*" is not an origin`},
		{origins: []string{"client.example"}, err: `--origin "client.example" is not an origin`},
		{origins: []string{"ftp://client.example"}, err: "is not an origin"},
		{origins: []string{"https:client.example"}, err: `--origin "https:client.example" is not an origin`},
		{origins: []string{`https://evil.example\@client.example`}, err: "--origin https://evil.example"},
		{origins: []string{"https://a@b@client.example"}, err: "--origin https://client.example"},
		{origins: []string{"https://ex%41mple.example"}, err: "is not an origin"},
		{origins: []string{"https://client .example"}, err: "is not an origin"},
		{hosts: []string{"box%2Eexample"}, err: "is not a host"},
		{hosts: []string{"$(id).example"}, err: `--host "$(id).example" is not a host`},
		{hosts: []string{"https://$(id).example/"}, err: "is not a host"},
		{origins: []string{"https://$(id).example"}, err: "is not an origin"},
		{origins: []string{"http://0x7f.1"}, err: "is not an origin"},
		{origins: []string{"https://[::1]:8443/app"}, err: "--origin 'https://[::1]:8443'"},
		{origins: []string{"https://client.example#"}, err: "--origin https://client.example"},
		{origins: []string{"https://café.example/"}, err: "xn--"},
		{hosts: []string{"café.example"}, err: "xn--"},
	} {
		if _, err := New(Config{Auth: store, Run: run, Hosts: c.hosts, Origins: c.origins}); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("hosts %q origins %q: %v, want %q", c.hosts, c.origins, err, c.err)
		}
	}
	c, err := store.Issue(context.Background(), "test client", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Auth: store, Run: run, Hosts: []string{"BOX.example:08443"}, Origins: []string{"HTTPS://Client.Example:0443/", "http://Page.Example:80", "http://page.example:8080", "https://[2001:0DB8:0:0:0:0:0:1]/", "http://[::ffff:192.0.2.1]:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://client.example", "http://page.example", "http://page.example:8080", "https://[2001:db8::1]", "http://[::ffff:c000:201]:8080"} {
		if w := call(s, "GET", "/v2/list", "", c.Token, "box.example:8443", origin); w.Code != 200 {
			t.Errorf("origin %s: %d %s", origin, w.Code, w.Body)
		}
	}
	if w := call(s, "GET", "/v2/list", "", c.Token, "box.example:8443", "https://client.example:8443"); w.Code != 403 {
		t.Errorf("another port of an allowed origin: %d", w.Code)
	}
	if w := call(s, "GET", "/v2/list", "", c.Token, "[::ffff:7f00:1]:7777", "http://[::ffff:7f00:1]:7777"); w.Code != 200 {
		t.Errorf("an IPv4-mapped loopback host and its own origin: %d %s", w.Code, w.Body)
	}
}

// Canonicalization applies to requests as well as configuration, without
// changing the port, scheme or credential gates or the origin echoed by CORS.
func TestCanonicalRequestsPreserveAdmissionGates(t *testing.T) {
	store := auth.Store{Dir: t.TempDir()}
	credential, err := store.Issue(context.Background(), "canonical request client", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		Auth:    store,
		Hosts:   []string{"box.example:8443", "[2001:db8::1]:8443", "tls.example:443", "[::ffff:192.0.2.1]:8443"},
		Origins: []string{"https://client.example", "https://[2001:db8::1]"},
		Run: func(_ context.Context, _ []string, _ io.Reader, out, _ io.Writer) int {
			_, _ = io.WriteString(out, `{"schema":"tmag/v2","sessions":[]}`)
			return 0
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, host, origin string
		unauthorized       bool
		status             int
	}{
		{name: "name and port", host: "BOX.EXAMPLE:08443", origin: "HTTPS://CLIENT.EXAMPLE:0443/", status: http.StatusOK},
		{name: "expanded IPv6", host: "[2001:0DB8:0:0:0:0:0:1]:08443", origin: "https://[2001:0DB8:0:0:0:0:0:1]:0443/", status: http.StatusOK},
		{name: "host keeps explicit default port", host: "TLS.EXAMPLE:00443", origin: "https://client.example:443", status: http.StatusOK},
		{name: "host default port cannot be omitted", host: "tls.example", origin: "https://client.example", status: http.StatusForbidden},
		{name: "mapped configured host uses browser hex", host: "[::ffff:c000:201]:8443", origin: "https://client.example", status: http.StatusOK},
		{name: "mapped configured host keeps port", host: "[::ffff:c000:201]", origin: "https://client.example", status: http.StatusForbidden},
		{name: "mapped loopback origin is the listener's own", host: "[::ffff:7f00:1]:7777", origin: "http://[::FFFF:127.0.0.1]:7777", status: http.StatusOK},
		{name: "same origin host default port", host: "LOCALHOST:00080", origin: "http://localhost", status: http.StatusOK},
		{name: "same origin origin default port", host: "localhost", origin: "http://LOCALHOST:00080/", status: http.StatusOK},
		{name: "different host port", host: "box.example:08444", origin: "https://client.example", status: http.StatusForbidden},
		{name: "different origin port", host: "box.example:8443", origin: "https://client.example:08444", status: http.StatusForbidden},
		{name: "different scheme", host: "box.example:8443", origin: "http://client.example:80", status: http.StatusForbidden},
		{name: "opaque origin", host: "box.example:8443", origin: "null", status: http.StatusForbidden},
		{name: "empty query", host: "box.example:8443", origin: "https://client.example?", status: http.StatusForbidden},
		{name: "fragment after slash", host: "box.example:8443", origin: "https://client.example/#", status: http.StatusForbidden},
		{name: "canonicalization needs credentials", host: "BOX.EXAMPLE:08443", origin: "HTTPS://CLIENT.EXAMPLE:0443/", unauthorized: true, status: http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			token := credential.Token
			if c.unauthorized {
				token = ""
			}
			w := call(s, "GET", "/v2/list", "", token, c.host, c.origin)
			if w.Code != c.status {
				t.Fatalf("status %d: %s; want %d", w.Code, w.Body, c.status)
			}
			if w.Code == http.StatusOK && w.Header().Get("Access-Control-Allow-Origin") != c.origin {
				t.Fatalf("CORS echoed %q; want request origin %q", w.Header().Get("Access-Control-Allow-Origin"), c.origin)
			}
		})
	}
}

// A refused Host or Origin is named in serve's log once, with the flag that
// would admit it when it could be the person's, and the log names a bounded
// number of them whatever reaches the listener.
func TestRefusalsAreNamedOnceWithTheFlagThatAdmits(t *testing.T) {
	store := auth.Store{Dir: t.TempDir()}
	c, err := store.Issue(context.Background(), "test client", 0)
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	s, err := New(Config{Auth: store, Run: func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { return 0 }, Log: log.New(&logged, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		host, origin, message string
	}{
		{"box.example.ts.net", "", "host is not allowed"},
		{"box.example.ts.net", "", "host is not allowed"},
		{"localhost", "https://Page.example", "origin is not allowed"},
		{"localhost:7777", "https://localhost:7777", "origin is not allowed"},
		{"localhost", "null", "origin is not allowed"},
		{"$(id).example", "", "host is not allowed"},
		{"localhost", "https://$(id).example", "origin is not allowed"},
		{"localhost", "https://[2001:db8::1]:8443", "origin is not allowed"},
		{strings.Repeat("h", 1000) + ".example", "", "host is not allowed"},
	} {
		w := call(s, "GET", "/v2/list", "", c.Token, v.host, v.origin)
		if w.Code != 403 || !strings.Contains(w.Body.String(), v.message) {
			t.Errorf("host %q origin %q: %d %s, want 403 %q", v.host, v.origin, w.Code, w.Body, v.message)
		}
	}
	lines := strings.Split(strings.TrimSuffix(logged.String(), "\n"), "\n")
	const keys = " (prefix + M-r takes its flags from @tmag-serve)"
	want := []string{
		`refused Host "box.example.ts.net": if it is your tunnel's host, start tmag serve with --host box.example.ts.net` + keys,
		`refused Origin "https://Page.example": if it is your client's page, start tmag serve with --origin https://page.example` + keys,
		`refused Origin "https://localhost:7777": if it is your client's page, start tmag serve with --origin https://localhost:7777` + keys,
		`refused Origin "null"`,
		`refused Host "$(id).example"`,
		`refused Origin "https://$(id).example"`,
		`refused Origin "https://[2001:db8::1]:8443": if it is your client's page, start tmag serve with --origin 'https://[2001:db8::1]:8443'` + keys,
		`refused Host "` + strings.Repeat("h", maxRefused) + `"`,
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("logged\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	for i := range 2 * maxRefusals {
		call(s, "GET", "/v2/list", "", c.Token, fmt.Sprintf("h%d.example", i), "")
	}
	lines = strings.Split(strings.TrimSuffix(logged.String(), "\n"), "\n")
	if len(lines) != maxRefusals+1 || !strings.HasSuffix(lines[maxRefusals], "the next ones go unnamed") {
		t.Fatalf("logged %d lines, ending %q; want %d refusals and a last line", len(lines), lines[len(lines)-1], maxRefusals)
	}
}

// A host is compared in the one form a browser writes it, whose characters a
// shell takes literally; anything else is no host.
func TestCanonicalHostIsTheFormBrowsersSend(t *testing.T) {
	for in, want := range map[string]string{
		"Box.Example.ts.net":             "box.example.ts.net",
		"box.example:08443":              "box.example:8443",
		"box.example:":                   "box.example",
		"my_box.local.":                  "my_box.local.",
		"127.0.0.1:7777":                 "127.0.0.1:7777",
		"[2001:0DB8::0001]:443":          "[2001:db8::1]:443",
		"[::1]":                          "[::1]",
		"localhost":                      "localhost",
		"1password.example":              "1password.example",
		"$(id).example":                  "",
		"a;b":                            "",
		"box example":                    "",
		"":                               "",
		"..":                             "",
		"1.2.3":                          "",
		"0x7f.1":                         "",
		"01.2.3.4":                       "",
		"::1":                            "",
		"[::ffff:127.0.0.1]":             "[::ffff:7f00:1]",
		"[::FFFF:7F00:1]:7777":           "[::ffff:7f00:1]:7777",
		"[fe80::1%25eth0]":               "",
		"[1.2.3.4]":                      "",
		"box.example:65536":              "",
		"box.example:+80":                "",
		"box.example:http":               "",
		strings.Repeat("a.", 127) + "aa": "",
	} {
		got, ok := canonicalHost(in)
		if got != want || ok != (want != "") {
			t.Errorf("canonicalHost(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
