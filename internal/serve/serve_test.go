package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
