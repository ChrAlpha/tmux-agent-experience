// Package gateway is tmag-client: a page for one person's phone, and the API it
// uses, over the tmag/v2 contract of each machine it is given. It follows each
// machine once, keeps its sessions and panes in memory, and serves every page
// from that state. Each action runs one tmag command, which checks its own gate.
package gateway

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/push"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/version"
)

//go:embed assets
var assets embed.FS

// Config is what tmag-client serves, and to whom.
type Config struct {
	// Machines are the machines whose agents the page shows; the first is
	// this one.
	Machines []Machine
	// Owner is who a tunnel must prove a request comes from: a Tailscale
	// login, or the email Cloudflare Access vouches for.
	Owner string
	// Tailscale are the host names Tailscale Serve forwards.
	Tailscale []string
	// Access is the application behind Cloudflare Access, if any.
	Access Access
	// StateDir holds the gateway's credentials, seen turns and VAPID keys.
	StateDir string
	// Push names the alert kinds to push. nil means the default, blocked and
	// finished; an empty list turns push off.
	Push []string
	// PushClient overrides the Web Push sender. The gateway fills its keys and
	// subject when they are empty; tests use it to reach a local service.
	PushClient *push.Client
	// Version is tmag-client's, shown on the page.
	Version string
	// Log gets one line per action: what, where, how it went, never content.
	Log *log.Logger
}

// Server serves the page and its API.
type Server struct {
	cfg    Config
	guard  guard
	auth   *auth.Store
	vapid  *push.Keys
	seen   *tracker
	hub    *hub
	states map[string]*machineState
	vis    *visibility
	push   *push.Client
	mux    *http.ServeMux
	kinds  map[string]bool
	public string // the first public host a tunnel serves
	queue  chan alertJob
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	close  sync.Once
}

// maxLine bounds one document of a watch stream; a snapshot of many
// sessions and panes is the largest.
const maxLine = 16 << 20

// pingEvery is how often an idle stream says it is still there, so that the
// page notices a connection that died without a word.
const pingEvery = 15 * time.Second

// actTimeout bounds one verb, which runs to its end even when the page that
// asked for it goes away.
const actTimeout = 2 * time.Minute

// New checks the configuration and returns the server, following every
// machine until Close.
func New(cfg Config) (*Server, error) {
	if len(cfg.Machines) == 0 {
		return nil, errors.New("no machine to show")
	}
	seen := map[string]bool{}
	for _, m := range cfg.Machines {
		if seen[m.Name] || len(m.Argv) == 0 {
			return nil, fmt.Errorf("machine %q named twice or without a command", m.Name)
		}
		seen[m.Name] = true
	}
	switch {
	case cfg.StateDir == "":
		return nil, errors.New("no state directory for the gateway's credentials")
	case len(cfg.Tailscale)+len(cfg.Access.Hosts) > 0 && cfg.Owner == "":
		return nil, errors.New("a tunnel needs --owner: who may come through it")
	case len(cfg.Access.Hosts) > 0 && (cfg.Access.Team == "" || cfg.Access.Audience == ""):
		return nil, errors.New("--access needs --access-team and --access-aud, which verify its tokens")
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	kinds := cfg.Push
	if kinds == nil {
		kinds = []string{"blocked", "finished"}
	}
	seenTurns, err := loadTracker(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:    cfg,
		guard:  guard{owner: cfg.Owner, tailscale: lower(cfg.Tailscale), access: lower(cfg.Access.Hosts)},
		auth:   &auth.Store{Dir: cfg.StateDir},
		seen:   seenTurns,
		hub:    newHub(),
		states: map[string]*machineState{},
		vis:    &visibility{at: map[string]time.Time{}},
		kinds:  map[string]bool{},
		public: publicHost(cfg),
		queue:  make(chan alertJob, 64),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if len(cfg.Access.Hosts) > 0 {
		s.guard.keys = newAccessKeys(cfg.Access)
	}
	for _, k := range kinds {
		if k = strings.TrimSpace(k); k != "" {
			s.kinds[k] = true
		}
	}
	if len(s.kinds) > 0 && s.public != "" {
		keys, err := push.LoadKeys(cfg.StateDir)
		if err != nil {
			s.cancel()
			return nil, err
		}
		s.vapid = keys
		s.push = cfg.PushClient
		if s.push == nil {
			s.push = &push.Client{}
		}
		if s.push.Keys == nil {
			s.push.Keys = keys
		}
		if s.push.Subject == "" {
			s.push.Subject = "https://" + s.public
		}
	}
	for _, m := range cfg.Machines {
		s.states[m.Name] = newMachineState(m.Name)
	}
	s.seen.live = s.live
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.asset)
	mux.HandleFunc("GET /app.js", s.asset)
	mux.HandleFunc("GET /app.css", s.asset)
	mux.HandleFunc("POST /api/pair", s.pairDevice)
	mux.HandleFunc("GET /api/machines", s.machines)
	mux.HandleFunc("GET /api/watch", s.watch)
	mux.HandleFunc("POST /api/seen", s.seenTurn)
	mux.HandleFunc("GET /api/push", s.pushInfo)
	mux.HandleFunc("POST /api/push", s.subscribe)
	mux.HandleFunc("DELETE /api/push", s.unsubscribe)
	mux.HandleFunc("POST /api/visible", s.reportVisible)
	mux.HandleFunc("GET /api/list", s.list)
	mux.HandleFunc("GET /api/request", s.request)
	mux.HandleFunc("GET /api/read", s.read)
	mux.HandleFunc("POST /api/act", s.act)
	s.mux = mux
	if len(s.kinds) > 0 && s.push != nil {
		s.wg.Add(1)
		go s.pushLoop()
	}
	s.wg.Add(1)
	go s.watchAuth()
	for _, m := range cfg.Machines {
		s.wg.Add(1)
		go s.follow(m)
	}
	return s, nil
}

// Close ends every follower and stream.
func (s *Server) Close() {
	s.close.Do(func() {
		s.hub.closeAll()
		s.cancel()
		s.wg.Wait()
		s.seen.flush()
	})
}

func publicHost(cfg Config) string {
	if len(cfg.Tailscale) > 0 {
		return cfg.Tailscale[0]
	}
	if len(cfg.Access.Hosts) > 0 {
		return cfg.Access.Hosts[0]
	}
	return ""
}

// public are served to anyone who asks under a known host name: browsers
// fetch them without credentials, and they hold nothing but the page's look.
var public = map[string]bool{"/manifest.webmanifest": true, "/icon.svg": true, "/icon.png": true, "/sw.js": true}

// page is the page itself, which holds no data either: it asks for a
// credential, or for a code to pair the device with, before it shows any.
var page = map[string]bool{"/": true, "/app.js": true, "/app.css": true, "/api/pair": true}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; script-src 'self'; style-src 'self'; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	if public[r.URL.Path] && r.Method == http.MethodGet {
		s.publicAsset(w, r)
		return
	}
	if err := s.guard.entry(r); err != nil {
		refuse(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	// A device without a credential, alone, is told so with its own status:
	// the page forgets one only then, not when a tunnel's check fails for a
	// moment.
	if page[r.URL.Path] {
		s.mux.ServeHTTP(w, r)
		return
	}
	credential, err := s.auth.Authenticate(r.Context(), presentedCredential(r))
	if err != nil {
		refuse(w, http.StatusUnauthorized, "credential", errCredential.Error())
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), credentialKey{}, credential))
	s.mux.ServeHTTP(w, r)
}

// presentedCredential is the device credential a request carries, as the
// Bearer token tmag serve also takes.
func presentedCredential(r *http.Request) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get(authorizationHeader)), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

type credentialKey struct{}

func credentialOf(r *http.Request) auth.Credential {
	c, _ := r.Context().Value(credentialKey{}).(auth.Credential)
	return c
}

// refuse answers with an error document, as tmag's commands do.
func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": version.State, "error": map[string]string{"code": code, "message": message}})
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	s.file(w, name)
}

// pairDevice gives a device its own credential in exchange for a code
// tmag-client shows or auth pair printed. A code works once.
func (s *Server) pairDevice(w http.ResponseWriter, r *http.Request) {
	if !jsonFromPage(w, r) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	issued, err := s.auth.Redeem(ctx, body.Code)
	if err != nil {
		s.cfg.Log.Printf("pairing refused through %q: %v", hostName(r.Host), err)
		refuse(w, http.StatusForbidden, "pairing", "that code is invalid, expired or already used")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"credential": issued})
}

// jsonFromPage admits a JSON body sent by the page itself: a browser sends
// none from another origin without asking first, and Origin, when present,
// must be this host's.
func jsonFromPage(w http.ResponseWriter, r *http.Request) bool {
	ctype, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ctype != "application/json" {
		http.Error(w, "a JSON body, please", http.StatusBadRequest)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return false
		}
	}
	return true
}

func (s *Server) publicAsset(w http.ResponseWriter, r *http.Request) {
	if hostName(r.Host) != "" && !loopback(hostName(r.Host)) && !slices.Contains(s.guard.tailscale, hostName(r.Host)) && !slices.Contains(s.guard.access, hostName(r.Host)) {
		http.Error(w, "forbidden: "+errHost.Error(), http.StatusForbidden)
		return
	}
	if r.URL.Path == "/icon.png" {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(iconPNG())
		return
	}
	s.file(w, strings.TrimPrefix(r.URL.Path, "/"))
}

func (s *Server) file(w http.ResponseWriter, name string) {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	ext := name[strings.LastIndexByte(name, '.'):]
	ctype := mime.TypeByExtension(ext)
	if ext == ".webmanifest" {
		ctype = "application/manifest+json"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	if name == "sw.js" {
		w.Header().Set("Service-Worker-Allowed", "/")
	}
	_, _ = w.Write(b)
}

// machines names the machines the page shows, this one first, and what this
// device may do with them.
func (s *Server) machines(w http.ResponseWriter, r *http.Request) {
	names := make([]string, len(s.cfg.Machines))
	for i, m := range s.cfg.Machines {
		names[i] = m.Name
	}
	credential := credentialOf(r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"machines":   names,
		"version":    s.cfg.Version,
		"seen":       s.seen.snapshot(),
		"push":       s.pushState(credential),
		"credential": map[string]string{"id": credential.ID, "label": credential.Label},
	})
}

func (s *Server) machine(w http.ResponseWriter, name string) (Machine, bool) {
	i := slices.IndexFunc(s.cfg.Machines, func(m Machine) bool { return m.Name == name })
	if i < 0 {
		http.Error(w, "unknown machine", http.StatusBadRequest)
		return Machine{}, false
	}
	return s.cfg.Machines[i], true
}

// watch serves the gateway's state as server-sent events: a "machine" event
// when a machine's stream starts or ends, a snapshot document for each
// machine, then a "doc" event for each change. A "seen" event reports a turn
// another device opened, and a "ping" event says the stream is alive.
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sub := s.hub.subscribe(credentialOf(r).ID, cancel)
	defer s.hub.unsubscribe(sub)

	_, _ = io.WriteString(w, ": watching\n\n")
	seen := s.seen.snapshot()
	for _, m := range s.cfg.Machines {
		state := s.states[m.Name]
		seq, sessions, panes, up, why := state.snapshot()
		s.seen.touch(m.Name, sessions)
		if _, err := io.WriteString(w, frame("machine", map[string]any{"machine": m.Name, "up": up, "error": why})); err != nil {
			return
		}
		doc := map[string]any{"schema": version.State, "type": "snapshot", "seq": seq, "sessions": sessions, "panes": panes, "seen": seen}
		if _, err := io.WriteString(w, frame("doc", map[string]any{"machine": m.Name, "doc": doc})); err != nil {
			return
		}
	}
	flusher.Flush()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		f, ok := next(ctx, sub.frames, ping.C)
		if !ok {
			return
		}
		if _, err := io.WriteString(w, f); err != nil {
			return
		}
		flusher.Flush()
	}
}

// seen marks the session's current turn seen, for every device.
func (s *Server) seenTurn(w http.ResponseWriter, r *http.Request) {
	if !jsonFromPage(w, r) {
		return
	}
	var body struct {
		Machine string `json:"machine"`
		Agent   string `json:"agent"`
		Session string `json:"session"`
		Turn    string `json:"turn"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	state, ok := s.states[body.Machine]
	if !ok {
		http.Error(w, "unknown machine", http.StatusBadRequest)
		return
	}
	session, ok := state.session(body.Agent, body.Session)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	turn := ""
	if body.Turn != "" && session.State == "idle" && session.Turn == body.Turn {
		turn = s.seen.mark(body.Machine, session.Agent, session.ID, session.Turn)
	}
	if turn != "" {
		s.hub.publish(frame("seen", map[string]string{"machine": body.Machine, "agent": session.Agent, "session": session.ID, "turn": turn}))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"turn": turn})
}

// pushState describes whether this device can turn push on.
func (s *Server) pushState(c auth.Credential) map[string]any {
	state := map[string]any{"enabled": false, "subscribed": c.Push, "key": ""}
	switch {
	case len(s.kinds) == 0:
		state["reason"] = "push is off"
	case s.public == "":
		state["reason"] = "push needs a tunnel: --tailscale or --access"
	case s.push == nil:
		state["reason"] = "push is unavailable"
	default:
		state["enabled"] = true
		state["key"] = s.vapid.Public()
	}
	return state
}

func (s *Server) pushInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.pushState(credentialOf(r)))
}

// subscribe stores this device's push subscription.
func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	if !jsonFromPage(w, r) {
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		P256DH   string `json:"p256dh"`
		Auth     string `json:"auth"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	p256dh, secret := body.Keys.P256DH, body.Keys.Auth
	if p256dh == "" {
		p256dh, secret = body.P256DH, body.Auth
	}
	sub := auth.Subscription{Endpoint: body.Endpoint, P256DH: p256dh, Auth: secret}
	check := push.Validate
	if s.push != nil {
		check = s.push.Validate
	}
	if _, err := check(push.Subscription{Endpoint: sub.Endpoint, P256DH: sub.P256DH, Auth: sub.Auth}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.auth.Subscribe(r.Context(), credentialOf(r).ID, sub); err != nil {
		refuse(w, http.StatusUnauthorized, "credential", errCredential.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"subscribed": true})
}

// unsubscribe drops this device's push subscription.
func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Unsubscribe(r.Context(), credentialOf(r).ID); err != nil {
		http.Error(w, "cannot drop the subscription", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"subscribed": false})
}

// reportVisible records whether this device's page shows.
func (s *Server) reportVisible(w http.ResponseWriter, r *http.Request) {
	if !jsonFromPage(w, r) {
		return
	}
	var body struct {
		Visible bool `json:"visible"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	s.vis.set(credentialOf(r).ID, body.Visible, time.Now())
	w.WriteHeader(http.StatusNoContent)
}

// list is `tmag list --json`, for checking what the page rebuilt.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	if m, ok := s.machine(w, r.URL.Query().Get("machine")); ok {
		s.relay(w, m.command(r.Context(), "list", "--json"))
	}
}

// request is `tmag request --json`: what an open request asks.
func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m, ok := s.machine(w, q.Get("machine"))
	if !ok {
		return
	}
	if q.Get("target") == "" || q.Get("request") == "" {
		http.Error(w, "request needs a target and a request", http.StatusBadRequest)
		return
	}
	s.relay(w, m.command(r.Context(), "request", "--json", "--", q.Get("target"), q.Get("request")))
}

// read is `tmag read --json`: a session's last reply with last=1, or a
// pane's recent text with lines=N.
func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m, ok := s.machine(w, q.Get("machine"))
	if !ok {
		return
	}
	args := []string{"read", "--json"}
	if q.Get("last") != "" {
		args = append(args, "--last")
	} else if n, err := strconv.Atoi(q.Get("lines")); err == nil && n > 0 && n <= 2000 {
		args = append(args, "--lines", strconv.Itoa(n))
	} else {
		http.Error(w, "read needs last=1 or lines=N (1 to 2000)", http.StatusBadRequest)
		return
	}
	if q.Get("target") == "" {
		http.Error(w, "read needs a target", http.StatusBadRequest)
		return
	}
	s.relay(w, m.command(r.Context(), append(args, "--", q.Get("target"))...))
}

// action is one control verb the page asks for.
type action struct {
	Machine  string            `json:"machine"`
	Verb     string            `json:"verb"` // send, interrupt or answer
	Target   string            `json:"target"`
	Text     string            `json:"text,omitempty"`     // send
	Request  string            `json:"request,omitempty"`  // answer
	Decision string            `json:"decision,omitempty"` // answer: allow or deny
	Message  string            `json:"message,omitempty"`  // answer
	Answers  map[string]string `json:"answers,omitempty"`  // answer, for a question
}

// act runs one control verb. Besides the device credential, it takes only a
// JSON body from the page's own origin.
func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	if !jsonFromPage(w, r) {
		return
	}
	var a action
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&a); err != nil || a.Target == "" {
		http.Error(w, "bad action", http.StatusBadRequest)
		return
	}
	m, ok := s.machine(w, a.Machine)
	if !ok {
		return
	}
	// The person pressed the button: a phone that loses its connection now
	// must not cut the verb off halfway, after tmag recorded it and before it
	// was delivered.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), actTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch a.Verb {
	case "send":
		cmd = m.command(ctx, "send", "--json", "--", a.Target, "-")
		cmd.Stdin = strings.NewReader(a.Text)
	case "interrupt":
		cmd = m.command(ctx, "interrupt", "--json", "--", a.Target)
	case "answer":
		args := []string{"answer", "--json"}
		if a.Message != "" {
			args = append(args, "--message="+a.Message)
		}
		if len(a.Answers) > 0 {
			b, _ := json.Marshal(a.Answers)
			args = append(args, "--answers="+string(b))
		}
		cmd = m.command(ctx, append(args, "--", a.Target, a.Request, a.Decision)...)
	default:
		http.Error(w, "unknown verb", http.StatusBadRequest)
		return
	}
	outcome := s.relay(w, cmd)
	// Quoted: the page's strings reach a terminal here, before tmag checks
	// them.
	what := a.Verb
	if a.Verb == "answer" {
		what += fmt.Sprintf(" %q %q", a.Decision, a.Request)
	}
	s.cfg.Log.Printf("%s %q on %q by credential %s: %q", what, a.Target, a.Machine, credentialOf(r).ID, outcome)
}

// relay runs a `tmag … --json` command and passes its document on: 200 when
// it succeeded, 422 with tmag's error document when it refused or failed. It
// describes the outcome for the log.
func (s *Server) relay(w http.ResponseWriter, cmd *exec.Cmd) string {
	stderr := &capped{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err == nil {
		_, _ = w.Write(out)
		return "ok"
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &doc) == nil && doc.Error.Code != "" {
		_, _ = w.Write(out)
		return doc.Error.Code
	}
	// No answer from tmag: the machine is out of reach, or runs no tmag.
	why := exitError(err, stderr.Bytes())
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": version.State, "error": map[string]string{"code": "error", "message": why}})
	return "error: " + why
}

// watchAuth closes streams and forgets visibility when a credential is
// revoked or expires. The store is shared with auth commands in other
// processes, so the gateway notices by reading it.
func (s *Server) watchAuth() {
	defer s.wg.Done()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			active, err := s.auth.Active(s.ctx)
			if err != nil {
				continue
			}
			s.hub.closeInactive(active)
			s.vis.keep(active)
		}
	}
}

func lower(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(n)
	}
	return out
}

// capped keeps the first 16 KiB written to it.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := 16<<10 - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
