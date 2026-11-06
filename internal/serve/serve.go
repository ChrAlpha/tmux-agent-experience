// Package serve is tmag serve: a page for one person's phone, and the API it
// uses, over the tmag/v1 contract of each machine it is given. It relays and
// keeps nothing: each page connection gets one `tmag watch` per machine, and
// each action runs one tmag command, which checks its own gate.
package serve

import (
	"bufio"
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
	"time"
)

//go:embed assets
var assets embed.FS

// Config is what tmag serve serves, and to whom.
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
	// Token admits requests made on this machine.
	Token string
	// Version is tmag's, shown on the page.
	Version string
	// Log gets one line per action: what, where, how it went, never content.
	Log *log.Logger
}

// Server serves the page and its API.
type Server struct {
	cfg   Config
	guard guard
	mux   *http.ServeMux
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

// New checks the configuration and returns the server.
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
	case cfg.Token == "":
		return nil, errors.New("no token for local access")
	case len(cfg.Tailscale)+len(cfg.Access.Hosts) > 0 && cfg.Owner == "":
		return nil, errors.New("a tunnel needs --owner: who may come through it")
	case len(cfg.Access.Hosts) > 0 && (cfg.Access.Team == "" || cfg.Access.Audience == ""):
		return nil, errors.New("--access needs --access-team and --access-aud, which verify its tokens")
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	s := &Server{cfg: cfg, guard: guard{owner: cfg.Owner, tailscale: lower(cfg.Tailscale), access: lower(cfg.Access.Hosts), token: cfg.Token}}
	if len(cfg.Access.Hosts) > 0 {
		s.guard.keys = newAccessKeys(cfg.Access)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /app.js", s.asset)
	mux.HandleFunc("GET /app.css", s.asset)
	mux.HandleFunc("GET /api/machines", s.machines)
	mux.HandleFunc("GET /api/watch", s.watch)
	mux.HandleFunc("GET /api/list", s.list)
	mux.HandleFunc("GET /api/request", s.request)
	mux.HandleFunc("GET /api/read", s.read)
	mux.HandleFunc("POST /api/act", s.act)
	s.mux = mux
	return s, nil
}

// public are served to anyone who asks under a known host name: browsers
// fetch them without credentials, and they hold nothing but the page's look.
var public = map[string]bool{"/manifest.webmanifest": true, "/icon.svg": true, "/icon.png": true}

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
	if err := s.guard.admit(r); err != nil {
		http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("token"); s.guard.isToken(t) {
		// Keep the token in the browser, and out of the address bar.
		http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.file(w, "index.html")
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	s.file(w, strings.TrimPrefix(r.URL.Path, "/"))
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
	_, _ = w.Write(b)
}

// machines names the machines the page shows, this one first.
func (s *Server) machines(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, len(s.cfg.Machines))
	for i, m := range s.cfg.Machines {
		names[i] = m.Name
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"machines": names, "version": s.cfg.Version})
}

func (s *Server) machine(w http.ResponseWriter, name string) (Machine, bool) {
	i := slices.IndexFunc(s.cfg.Machines, func(m Machine) bool { return m.Name == name })
	if i < 0 {
		http.Error(w, "unknown machine", http.StatusBadRequest)
		return Machine{}, false
	}
	return s.cfg.Machines[i], true
}

// watch relays `tmag watch` of every machine as server-sent events: a
// "machine" event when a machine's stream starts or ends, a "doc" event for
// each of its documents, unchanged, under the machine's name, and a "ping"
// event when there was nothing else to say for a while. A machine whose
// stream ends is watched again, resumed after the last sequence number
// relayed; the page reconnects with ?since=NAME:SEQ,… after losing it all.
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	ctx := r.Context()
	frames := make(chan string, 64)
	for _, m := range s.cfg.Machines {
		from, ok := since[m.Name]
		if !ok {
			from = -1
		}
		go s.follow(ctx, m, from, frames)
	}
	_, _ = io.WriteString(w, ": watching\n\n")
	flusher.Flush()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		var frame string
		select {
		case <-ctx.Done():
			return
		case frame = <-frames:
		case <-ping.C:
			// A comment would keep proxies from timing out too, but a page
			// sees only events: this one tells it the stream is alive.
			frame = "event: ping\ndata: 1\n\n"
		}
		if _, err := io.WriteString(w, frame); err != nil {
			return
		}
		flusher.Flush()
	}
}

// follow runs `tmag watch` on one machine for as long as ctx lasts, again
// whenever it ends, from the last sequence number it relayed.
func (s *Server) follow(ctx context.Context, m Machine, since int64, frames chan<- string) {
	name, _ := json.Marshal(m.Name)
	send := func(frame string) bool {
		select {
		case frames <- frame:
			return true
		case <-ctx.Done():
			return false
		}
	}
	state := func(up bool, why string) bool {
		doc, _ := json.Marshal(map[string]any{"machine": m.Name, "up": up, "error": why})
		return send("event: machine\ndata: " + string(doc) + "\n\n")
	}
	for backoff := time.Second; ; backoff = min(2*backoff, 30*time.Second) {
		run, stop := context.WithCancel(ctx)
		args := []string{"watch"}
		if since >= 0 {
			args = append(args, "--since", strconv.FormatInt(since, 10))
		}
		cmd := m.command(run, args...)
		stderr := &capped{}
		cmd.Stderr = stderr
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			up := false
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 64<<10), maxLine)
			for sc.Scan() {
				line := sc.Bytes()
				// A line that is no JSON document is dropped: it could
				// otherwise end an event early, \r being a line end too.
				if !json.Valid(line) {
					continue
				}
				if !up {
					up, backoff = true, time.Second
					if !state(true, "") {
						break
					}
				}
				since = seqOf(line, since)
				if !send("event: doc\ndata: {\"machine\":" + string(name) + ",\"doc\":" + string(line) + "}\n\n") {
					break
				}
			}
			stop() // a stream left unread would block tmag forever
			err = cmd.Wait()
			if err == nil {
				err = errors.New("tmag watch ended")
			}
		}
		stop()
		if ctx.Err() != nil || !state(false, exitError(err, stderr.Bytes())) {
			return
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}
}

// seqOf is the sequence number a watch document brings the stream to.
func seqOf(line []byte, since int64) int64 {
	var doc struct {
		Type  string `json:"type"`
		Seq   int64  `json:"seq"`
		Event *struct {
			Seq int64 `json:"seq"`
		} `json:"event"`
	}
	if json.Unmarshal(line, &doc) != nil {
		return since
	}
	switch {
	case doc.Type == "snapshot":
		return doc.Seq
	case doc.Type == "event" && doc.Event != nil:
		return doc.Event.Seq
	}
	return since
}

// parseSince reads NAME:SEQ,… into a map.
func parseSince(s string) (map[string]int64, error) {
	since := map[string]int64{}
	for _, f := range strings.Split(s, ",") {
		if f == "" {
			continue
		}
		name, seq, ok := strings.Cut(f, ":")
		n, err := strconv.ParseInt(seq, 10, 64)
		if !ok || err != nil || n < 0 {
			return nil, fmt.Errorf("since: %q is not NAME:SEQ", f)
		}
		since[name] = n
	}
	return since, nil
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

// act runs one control verb. It takes only a JSON body with a custom header
// from the page's own origin, which a page elsewhere cannot make a browser
// send.
func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	ctype, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Header.Get("X-Tmag-Action") != "1" || ctype != "application/json" {
		http.Error(w, "an action takes a JSON body and the X-Tmag-Action header", http.StatusBadRequest)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "forbidden: cross-origin action", http.StatusForbidden)
			return
		}
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
	what := a.Verb
	if a.Verb == "answer" {
		what += " " + a.Decision + " " + a.Request
	}
	s.cfg.Log.Printf("%s %s on %s: %s", what, a.Target, a.Machine, outcome)
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
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": "tmag/v1", "error": map[string]string{"code": "error", "message": why}})
	return "error: " + why
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
