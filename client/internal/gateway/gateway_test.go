package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/devices"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/webpush/webpushtest"
)

// backend is a machine the test drives: it hands the gateway the documents
// the test sends, and answers verbs as the test says.
type backend struct {
	docs   chan machine.Doc
	breaks chan error
	mu     sync.Mutex
	sinces []int64
	calls  []machine.Op
	answer func(machine.Op) (json.RawMessage, error)
}

func newBackend() *backend {
	return &backend{docs: make(chan machine.Doc), breaks: make(chan error), answer: func(machine.Op) (json.RawMessage, error) {
		return json.RawMessage(`{"schema":"tmag/v2","via":"inbox"}`), nil
	}}
}

func (b *backend) Watch(ctx context.Context, since int64, each func(machine.Doc) error) error {
	b.mu.Lock()
	b.sinces = append(b.sinces, since)
	b.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-b.breaks:
			return err
		case d := <-b.docs:
			if err := each(d); err != nil {
				return err
			}
		}
	}
}

func (b *backend) Call(_ context.Context, op machine.Op) (json.RawMessage, error) {
	b.mu.Lock()
	b.calls = append(b.calls, op)
	answer := b.answer
	b.mu.Unlock()
	return answer(op)
}

func (b *backend) lastCall() machine.Op {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[len(b.calls)-1]
}

type harness struct {
	t       *testing.T
	g       *Gateway
	srv     *httptest.Server
	box     *backend
	store   *devices.Store
	dir     string
	token   string
	device  devices.Device
	pushes  chan *http.Request
	pushURL string
	cancel  context.CancelFunc
}

// start runs a gateway over one machine, box, reached at a public origin,
// with one paired device.
func start(t *testing.T, opts ...func(*Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := devices.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := &harness{t: t, box: newBackend(), store: store, dir: dir, pushes: make(chan *http.Request, 16)}
	push := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		w.pushes <- r
		if strings.HasSuffix(r.URL.Path, "/gone") {
			rw.WriteHeader(http.StatusGone)
			return
		}
		rw.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(push.Close)
	public, _ := url.Parse("https://box.example.ts.net")
	cfg := Config{
		Machines: []machine.Machine{{Name: "box", Backend: w.box}}, Public: []*url.URL{public},
		Devices: store, StateDir: dir, Push: []string{"blocked", "finished"},
		PushServices: []string{strings.TrimPrefix(push.URL, "https://")}, PushHTTP: push.Client(), Version: "test",
	}
	for _, o := range opts {
		o(&cfg)
	}
	w.g, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	done := make(chan struct{})
	go func() { w.g.Run(ctx); close(done) }()
	w.srv = httptest.NewServer(w.g)
	t.Cleanup(func() { cancel(); w.srv.Close(); <-done })
	w.device, w.token = w.pair("phone")
	w.pushURL = push.URL
	return w
}

func (w *harness) pair(label string) (devices.Device, string) {
	inv, err := w.store.Invite(label, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	d, token, err := w.store.Redeem(inv.Code, "")
	if err != nil {
		w.t.Fatal(err)
	}
	return d, token
}

func (w *harness) do(method, path, token string, body any) (int, map[string]any) {
	w.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, w.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// page is a page's event stream.
type page struct {
	t      *testing.T
	frames chan frame
	closed chan struct{}
	cancel context.CancelFunc
}

type frame struct {
	event string
	data  map[string]any
}

func (w *harness) open(token string) *page {
	w.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.srv.URL+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		w.t.Fatalf("events: %v %v", err, resp)
	}
	p := &page{t: w.t, frames: make(chan frame, 256), closed: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(p.closed)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		ev := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				var m map[string]any
				_ = json.Unmarshal([]byte(v), &m)
				p.frames <- frame{ev, m}
			}
		}
	}()
	w.t.Cleanup(cancel)
	return p
}

// until reads frames until one of event satisfies ok.
func (p *page) until(event string, ok func(map[string]any) bool) map[string]any {
	p.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.event == event && (ok == nil || ok(f.data)) {
				return f.data
			}
		case <-p.closed:
			p.t.Fatalf("the stream ended waiting for %s", event)
		case <-deadline:
			p.t.Fatalf("no %s frame", event)
		}
	}
}

// none checks that no frame of event satisfying ok comes within a moment.
func (p *page) none(event string, ok func(map[string]any) bool) {
	p.t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case f := <-p.frames:
			if f.event == event && (ok == nil || ok(f.data)) {
				p.t.Fatalf("unexpected %s frame %v", event, f.data)
			}
		case <-deadline:
			return
		}
	}
}

func (p *page) ended() bool {
	select {
	case <-p.closed:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func keyIs(k string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["key"] == k }
}

const s1 = "box/claude/s1"

func snapshot(sessions ...machine.Session) machine.Doc {
	return machine.Doc{Type: "snapshot", Seq: 10, Sessions: sessions, Panes: []machine.Pane{
		{Server: "/tmp/tmux-1000/default", ID: "%3", Target: "work:1.0", Agent: "claude", Session: "s1", View: "main"},
	}}
}

func event(seq int64, name, state, turn string, requests ...machine.Request) machine.Doc {
	return machine.Doc{Type: "event", Event: &machine.Event{Seq: seq, Time: time.Now(), Agent: "claude", Session: "s1", Event: name,
		State: state, Turn: turn, Cwd: "/home/me/src/api", Capabilities: []string{"send", "interrupt", "answer"}, Requests: requests}}
}

func alertDoc(seq int64, a machine.Alert, at time.Time) machine.Doc {
	return machine.Doc{Type: "event", Event: &machine.Event{Seq: seq, Time: at, Agent: "claude", Session: "s1", Event: "Alert", Alert: &a}}
}

var idle1 = machine.Session{ID: "s1", Agent: "claude", State: "idle", Turn: "t1", Cwd: "/home/me/src/api", Capabilities: []string{"send"}}

func TestAPageFollowsTheMachine(t *testing.T) {
	w := start(t)
	p := w.open(w.token)
	p.until("hello", nil)
	p.until("machine", func(m map[string]any) bool { return m["status"] == Connecting })
	p.until("ready", nil)
	w.box.docs <- snapshot(idle1)
	p.until("machine", func(m map[string]any) bool { return m["status"] == Online })
	v := p.until("session", keyIs(s1))
	if v["unread"] != true || v["panes"].([]any)[0].(map[string]any)["target"] != "work:1.0" {
		t.Fatalf("view %v", v)
	}
	w.box.docs <- event(11, "UserPromptSubmit", "working", "t2")
	p.until("session", func(m map[string]any) bool { return m["state"] == "working" && m["unread"] == false })
	// The same state again sends nothing.
	w.box.docs <- event(12, "PreToolUse", "working", "t2")
	p.none("session", nil)
	w.box.docs <- event(13, "SessionEnd", "", "")
	p.until("gone", keyIs(s1))

	// A broken watch marks the machine offline and resumes after the last
	// event.
	w.box.breaks <- &machine.Error{Code: machine.Unreachable, Message: "Connection refused"}
	p.until("machine", func(m map[string]any) bool { return m["status"] == Offline && m["error"] == "Connection refused" })
	w.box.docs <- event(14, "SessionStart", "idle", "")
	p.until("machine", func(m map[string]any) bool { return m["status"] == Online })
	w.box.mu.Lock()
	sinces := w.box.sinces
	w.box.mu.Unlock()
	if len(sinces) != 2 || sinces[0] != -1 || sinces[1] != 13 {
		t.Fatalf("watches since %v", sinces)
	}

	// A page that joins later gets everything at once.
	q := w.open(w.token)
	q.until("hello", nil)
	q.until("session", func(m map[string]any) bool { return m["key"] == s1 && m["unread"] == false })
	q.until("ready", nil)
}

func TestReadingATurnSettlesItForEveryPage(t *testing.T) {
	w := start(t)
	p := w.open(w.token)
	w.box.docs <- snapshot(idle1)
	p.until("session", func(m map[string]any) bool { return m["unread"] == true })
	if code, _ := w.do("POST", "/api/read", w.token, map[string]string{"key": s1, "turn": "t0"}); code != http.StatusConflict {
		t.Fatalf("an old turn read: %d", code)
	}
	if code, _ := w.do("POST", "/api/read", w.token, map[string]string{"key": s1, "turn": "t1"}); code != http.StatusNoContent {
		t.Fatalf("read: %d", code)
	}
	p.until("session", func(m map[string]any) bool { return m["unread"] == false })
	// A turn still running is never read.
	w.box.docs <- event(11, "UserPromptSubmit", "working", "t2")
	p.until("session", func(m map[string]any) bool { return m["state"] == "working" })
	if code, _ := w.do("POST", "/api/read", w.token, map[string]string{"key": s1, "turn": "t2"}); code != http.StatusConflict {
		t.Fatalf("a running turn read: %d", code)
	}
	w.box.docs <- event(12, "Stop", "idle", "t2")
	p.until("session", func(m map[string]any) bool { return m["unread"] == true })

	// What was read stays read for the next gateway.
	seen, err := loadSeen(w.dir)
	if err != nil || seen.turn(s1) != "t1" {
		t.Fatalf("kept %q %v", seen.turn(s1), err)
	}
}

func TestSeenTurnsAreBounded(t *testing.T) {
	s, _ := loadSeen(t.TempDir())
	at := time.Unix(0, 0)
	s.now = func() time.Time { at = at.Add(time.Second); return at }
	for i := range seenLimit + 10 {
		_ = s.mark("box/claude/"+string(rune('a'+i%26))+strings.Repeat("x", i/26), "t")
	}
	if len(s.turns) != seenLimit || s.turn("box/claude/a") != "" {
		t.Fatalf("%d turns kept, the oldest %q", len(s.turns), s.turn("box/claude/a"))
	}
}

func TestAlertsReachPagesAndAbsentDevices(t *testing.T) {
	w := start(t)
	away, awayToken := w.pair("tablet")
	_ = awayToken
	if err := w.store.Subscribe(w.device.ID, w.subscription("/phone")); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Subscribe(away.ID, w.subscription("/tablet")); err != nil {
		t.Fatal(err)
	}
	p := w.open(w.token)
	// The phone's page is on screen: it gets the alert in place, no push.
	if code, _ := w.do("POST", "/api/presence", w.token, map[string]bool{"visible": true}); code != http.StatusNoContent {
		t.Fatalf("presence: %d", code)
	}
	req := machine.Request{ID: "r1", Kind: "permission", Tool: "Bash", Since: time.Now()}
	w.box.docs <- snapshot(idle1)
	w.box.docs <- event(11, "PermissionRequest", "blocked", "t2", req)
	w.box.docs <- alertDoc(12, machine.Alert{Kind: "blocked", Request: &req}, time.Now())
	n := p.until("alert", nil)
	if n["title"] != "claude needs your approval" || n["body"] != "Bash · api · box" || n["url"] != "/#/s/"+s1 {
		t.Fatalf("notice %v", n)
	}
	got := w.push(t)
	if !strings.HasSuffix(got.URL.Path, "/tablet") || got.Header.Get("Urgency") != "high" || len(got.Header.Get("Topic")) != 32 {
		t.Fatalf("push %s %v", got.URL, got.Header)
	}
	w.noPush(t)

	// At a terminal that shows the session: pages only.
	w.box.docs <- alertDoc(13, machine.Alert{Kind: "blocked", Request: &req, Present: true}, time.Now())
	p.until("alert", nil)
	w.noPush(t)
	// A request that closed before its alert arrived: nothing.
	w.box.docs <- event(14, "Answer", "working", "t2")
	w.box.docs <- alertDoc(15, machine.Alert{Kind: "blocked", Request: &req}, time.Now())
	p.none("alert", nil)
	w.noPush(t)
	// A finished turn replayed from long ago reaches pages, not devices.
	w.box.docs <- event(16, "Stop", "idle", "t2")
	w.box.docs <- alertDoc(17, machine.Alert{Kind: "finished", Turn: "t2"}, time.Now().Add(-time.Hour))
	p.until("alert", func(m map[string]any) bool { return m["title"] == "claude finished" })
	w.noPush(t)
	// A turn already read: nothing.
	w.do("POST", "/api/read", w.token, map[string]string{"key": s1, "turn": "t2"})
	w.box.docs <- alertDoc(18, machine.Alert{Kind: "finished", Turn: "t2"}, time.Now())
	p.none("alert", nil)
	w.noPush(t)
}

func TestAGoneSubscriptionIsDropped(t *testing.T) {
	w := start(t)
	_ = w.store.Subscribe(w.device.ID, w.subscription("/gone"))
	w.box.docs <- snapshot(idle1)
	w.box.docs <- event(11, "UserPromptSubmit", "working", "t2")
	w.box.docs <- event(12, "Stop", "idle", "t2")
	w.box.docs <- alertDoc(13, machine.Alert{Kind: "finished", Turn: "t2"}, time.Now())
	w.push(t)
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, _, _ := w.store.Device(w.device.ID)
		if d.Push == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the subscription stayed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPushesCarryMetadataOnly(t *testing.T) {
	w := start(t)
	browser, sub := w.browser("/phone")
	_ = w.store.Subscribe(w.device.ID, sub)
	w.box.docs <- snapshot(idle1)
	w.box.docs <- event(11, "UserPromptSubmit", "working", "t2")
	w.box.docs <- event(12, "Stop", "idle", "t2")
	w.box.docs <- alertDoc(13, machine.Alert{Kind: "finished", Turn: "t2"}, time.Now())
	got := w.push(t)
	body, _ := io.ReadAll(got.Body)
	var n map[string]any
	plain, err := browser.Open(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plain, &n); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"kind": "finished", "key": s1, "machine": "box", "agent": "claude", "session": "s1", "dir": "api",
		"title": "claude finished", "body": "api · box", "tag": tag(s1), "url": "/#/s/" + s1}
	if len(n) != len(want) {
		t.Fatalf("payload %v", n)
	}
	for k, v := range want {
		if n[k] != v {
			t.Fatalf("payload %s = %v, want %v", k, n[k], v)
		}
	}
}

func TestAdmission(t *testing.T) {
	w := start(t)
	get := func(host string) int {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for host, want := range map[string]int{"127.0.0.1:7778": 200, "localhost:7778": 200, "[::1]:7778": 200, "box.example.ts.net": 200, "evil.example": 403, "box.example.ts.net.evil.example": 403} {
		if got := get(host); got != want {
			t.Errorf("host %s: %d, want %d", host, got, want)
		}
	}
	if code, _ := w.do("GET", "/api/devices", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", code)
	}
	if code, _ := w.do("GET", "/api/devices", "tmagc_forged", nil); code != http.StatusUnauthorized {
		t.Fatalf("a forged credential: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/api/presence", strings.NewReader(`{"visible":true}`))
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another site's request: %d", resp.StatusCode)
	}

	inv, _ := w.store.Invite("laptop", 0)
	code, out := w.do("POST", "/api/pair", "", map[string]string{"code": inv.Code})
	if code != http.StatusOK || !strings.HasPrefix(out["token"].(string), "tmagc_") {
		t.Fatalf("pair: %d %v", code, out)
	}
	for i := range 12 {
		code, _ := w.do("POST", "/api/pair", "", map[string]string{"code": "AAAA-AAAA-AAAA"})
		if want := map[bool]int{true: http.StatusForbidden, false: http.StatusTooManyRequests}[i < 10]; code != want {
			t.Fatalf("wrong code %d: %d, want %d", i, code, want)
		}
	}
}

func TestRevocationEndsAStream(t *testing.T) {
	w := start(t)
	p := w.open(w.token)
	p.until("ready", nil)
	other, _ := devices.Open(w.dir) // tmag-client auth revoke, in another process
	if err := other.Revoke(w.device.ID); err != nil {
		t.Fatal(err)
	}
	if !p.ended() {
		t.Fatal("a revoked device's stream goes on")
	}
	d, token := w.pair("laptop")
	q := w.open(token)
	q.until("ready", nil)
	_, admin := w.pair("admin")
	if code, _ := w.do("DELETE", "/api/devices/"+d.ID, admin, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if !q.ended() {
		t.Fatal("a stream goes on after its device was revoked from a page")
	}
	if code, _ := w.do("GET", "/api/devices", token, nil); code != http.StatusUnauthorized {
		t.Fatalf("a revoked credential: %d", code)
	}
}

func TestActionsAreTmagVerbs(t *testing.T) {
	w := start(t)
	p := w.open(w.token)
	w.box.docs <- snapshot(idle1)
	p.until("session", keyIs(s1))
	code, out := w.do("POST", "/api/send", w.token, map[string]string{"key": s1, "text": "go on"})
	if code != http.StatusOK || out["via"] != "inbox" {
		t.Fatalf("send: %d %v", code, out)
	}
	if op := w.box.lastCall(); op.Verb != "send" || op.Target != "s1" || op.Text != "go on" {
		t.Fatalf("send ran %+v", op)
	}
	w.box.answer = func(machine.Op) (json.RawMessage, error) {
		return nil, &machine.Error{Code: "refused:changed", Message: "the request closed", Next: "read the session again"}
	}
	code, out = w.do("POST", "/api/answer", w.token, map[string]any{"key": s1, "request": "r1", "decision": "deny", "message": "no"})
	e, _ := out["error"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "refused:changed" || e["next"] != "read the session again" {
		t.Fatalf("refusal: %d %v", code, out)
	}
	if op := w.box.lastCall(); op.Request != "r1" || op.Decision != "deny" || op.Message != "no" {
		t.Fatalf("answer ran %+v", op)
	}
	w.box.answer = func(op machine.Op) (json.RawMessage, error) {
		return json.RawMessage(`{"schema":"tmag/v2","text":"$ make test\nok"}`), nil
	}
	code, out = w.do("GET", "/api/screen?lines=80&key="+url.QueryEscape(s1), w.token, nil)
	if code != http.StatusOK || out["text"] != "$ make test\nok" || out["target"] != "work:1.0" {
		t.Fatalf("screen: %d %v", code, out)
	}
	if op := w.box.lastCall(); op.Target != "/tmp/tmux-1000/default:%3" || op.Lines != 80 {
		t.Fatalf("screen read %+v", op)
	}
	if code, _ := w.do("POST", "/api/send", w.token, map[string]string{"key": "box/claude/none", "text": "x"}); code != http.StatusNotFound {
		t.Fatalf("an unknown session: %d", code)
	}
	if code, _ := w.do("POST", "/api/send", w.token, map[string]string{"key": s1, "text": "  "}); code != http.StatusBadRequest {
		t.Fatalf("an empty prompt: %d", code)
	}
}

func TestAStreamThatFallsBehindEnds(t *testing.T) {
	s := newStream("d")
	for range streamQueue + 1 {
		s.send("session", []byte("{}"))
	}
	select {
	case <-s.done:
	default:
		t.Fatal("an overflowing stream stays open")
	}
}

func TestPushNeedsAPublicOrigin(t *testing.T) {
	w := start(t, func(c *Config) { c.Public = nil })
	_, out := w.do("GET", "/api/push", w.token, nil)
	if out["reason"] != "no_public" || out["key"] != nil {
		t.Fatalf("push info %v", out)
	}
	w = start(t)
	_, out = w.do("GET", "/api/push", w.token, nil)
	if k, _ := out["key"].(string); len(k) != 87 {
		t.Fatalf("push info %v", out)
	}
	code, _ := w.do("PUT", "/api/push", w.token, map[string]any{"endpoint": "https://example.com/push", "keys": map[string]string{"p256dh": "a", "auth": "b"}})
	if code != http.StatusBadRequest {
		t.Fatalf("a push endpoint elsewhere: %d", code)
	}
}

func (w *harness) push(t *testing.T) *http.Request {
	t.Helper()
	select {
	case r := <-w.pushes:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
		return nil
	}
}

func (w *harness) noPush(t *testing.T) {
	t.Helper()
	select {
	case r := <-w.pushes:
		t.Fatalf("an unexpected push to %s", r.URL)
	case <-time.After(300 * time.Millisecond):
	}
}

// subscription is a device's subscription at the fake push service.
func (w *harness) subscription(path string) devices.Subscription {
	_, sub := w.browser(path)
	return sub
}

func (w *harness) browser(path string) (*webpushtest.Receiver, devices.Subscription) {
	r, p256dh, auth := webpushtest.New()
	return r, devices.Subscription{Endpoint: w.pushURL + path, P256DH: p256dh, Auth: auth}
}
