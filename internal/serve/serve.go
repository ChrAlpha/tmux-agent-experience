// Package serve exposes one local tmag instance over the versioned HTTP
// contract. It owns no UI, remote machine inventory or agent state.
package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/transcript"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

const Schema = "tmag/http/v2"
const actTimeout = 2 * time.Minute

// Runner runs an allowlisted local CLI operation in process, with the owner's
// validated authority. It must honor ctx, including a watch's cancellation.
type Runner func(context.Context, []string, io.Reader, io.Writer, io.Writer) int

type Config struct {
	Auth    auth.Store
	Run     Runner
	Hosts   []string // additional HTTP Host values; loopback is accepted by default
	Origins []string // additional browser origins, compared as a browser sends them
	Version string
	Log     *log.Logger
}
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	refused refusals
}

func New(cfg Config) (*Server, error) {
	if cfg.Auth.Dir == "" || cfg.Run == nil {
		return nil, errors.New("serve needs an authorization store and a local runner")
	}
	hosts := make([]string, len(cfg.Hosts))
	for i, h := range cfg.Hosts {
		host, err := allowedHost(h)
		if err != nil {
			return nil, err
		}
		hosts[i] = host
	}
	origins := make([]string, len(cfg.Origins))
	for i, o := range cfg.Origins {
		origin, err := allowedOrigin(o)
		if err != nil {
			return nil, err
		}
		origins[i] = origin
	}
	cfg.Hosts, cfg.Origins = hosts, origins
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v2/info", s.info)
	s.mux.HandleFunc("GET /v2/list", s.list)
	s.mux.HandleFunc("GET /v2/inspect", s.inspect)
	s.mux.HandleFunc("GET /v2/request", s.request)
	s.mux.HandleFunc("GET /v2/read", s.read)
	s.mux.HandleFunc("GET /v2/changes", s.changes)
	s.mux.HandleFunc("GET /v2/dirs", s.dirs)
	s.mux.HandleFunc("GET /v2/watch", s.watch)
	s.mux.HandleFunc("POST /v2/act", s.act)
	return s, nil
}

// canonicalHost is h, a host and maybe a port, in the one form a browser
// writes it in Host and Origin: a name in lower case, an IP address in its
// shortest form (an IPv4-mapped IPv6 one in hex, as browsers write it), a
// port as a number. A name holds letters, digits, hyphens,
// underscores and dots only, and its last label is no number, which a browser
// would read as an IPv4 address; ok is false for anything else. So a host in
// this form is also a word a shell takes literally, apart from an IPv6
// address's brackets.
func canonicalHost(h string) (string, bool) {
	name, port := h, ""
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		name, port = h[:i], h[i+1:]
	}
	if inner, ok := strings.CutPrefix(name, "["); ok {
		inner, closed := strings.CutSuffix(inner, "]")
		ip, err := netip.ParseAddr(inner)
		if !closed || err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", false
		}
		name = "[" + ip.String() + "]"
		if ip.Is4In6() {
			b := ip.As16()
			name = fmt.Sprintf("[::ffff:%x:%x]", uint16(b[12])<<8|uint16(b[13]), uint16(b[14])<<8|uint16(b[15]))
		}
	} else if ip, err := netip.ParseAddr(name); err == nil && ip.Is4() {
		name = ip.String()
	} else if hostName(name) {
		name = strings.ToLower(name)
	} else {
		return "", false
	}
	if port == "" {
		return name, true
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > 65535 || strings.Trim(port, "0123456789") != "" {
		return "", false
	}
	return name + ":" + strconv.Itoa(n), true
}

// hostName reports whether s is a host name canonicalHost takes.
func hostName(s string) bool {
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	for _, l := range labels {
		if l == "" || strings.Trim(l, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			return false
		}
	}
	last := strings.ToLower(labels[len(labels)-1])
	hex, isHex := strings.CutPrefix(last, "0x")
	return strings.Trim(last, "0123456789") != "" && (!isHex || strings.Trim(hex, "0123456789abcdef") != "")
}

// defaultPort is the suffix a scheme's default port leaves on a canonical
// host, which Host and Origin omit.
var defaultPort = map[string]string{"http": ":80", "https": ":443"}

// splitURL splits an http or https URL into its scheme in lower case, its
// host and port, whether userinfo preceded them, and whatever follows them,
// which a backslash starts as a slash does, as browsers read these schemes.
// It is this small on purpose: what an origin may hold is canonicalHost's to
// say, whatever the Go release's URL parser takes.
func splitURL(s string) (scheme, host string, userinfo bool, rest string, ok bool) {
	scheme, after, ok := strings.Cut(s, "://")
	scheme = strings.ToLower(scheme)
	if !ok || (scheme != "http" && scheme != "https") {
		return "", "", false, "", false
	}
	end := strings.IndexAny(after, "/\\?#")
	if end < 0 {
		end = len(after)
	}
	host, rest = after[:end], after[end:]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host, userinfo = host[at+1:], true
	}
	return scheme, host, userinfo, rest, true
}

// urlHost is a URL's host as its Host header and Origin carry it.
func urlHost(scheme, host string) (string, bool) {
	host, ok := canonicalHost(host)
	return strings.TrimSuffix(host, defaultPort[scheme]), ok
}

// shellWord is a canonical host or origin as a word a shell takes literally.
func shellWord(s string) string {
	if strings.ContainsAny(s, "[]") {
		return "'" + s + "'"
	}
	return s
}

// asciiHint tells how to write a host name that is not in ASCII: a browser
// sends its ASCII form, which only a mapping tmag does not carry derives.
func asciiHint(s string) string {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return "; write an internationalized name in the ASCII form a browser sends, xn--..."
		}
	}
	return ""
}

// allowedHost is h, an allowed Host header value, in canonicalHost's form. A
// URL pasted in its place is refused with the host it names.
func allowedHost(h string) (string, error) {
	if scheme, host, _, _, ok := splitURL(h); ok {
		if host, ok := urlHost(scheme, host); ok {
			return "", fmt.Errorf("--host takes a host, not the URL %q: --host %s", h, shellWord(host))
		}
	}
	host, ok := canonicalHost(h)
	if !ok {
		return "", fmt.Errorf("--host %q is not a host, such as box.example.ts.net%s", h, asciiHint(h))
	}
	return host, nil
}

// allowedOrigin is the origin o names, as a browser sends it in Origin: its
// scheme and its host in canonicalHost's form, without the scheme's default
// port or a trailing slash. Anything more after the host is refused with that
// origin.
func allowedOrigin(o string) (string, error) {
	scheme, host, userinfo, rest, ok := splitURL(o)
	if ok {
		host, ok = urlHost(scheme, host)
	}
	if !ok {
		return "", fmt.Errorf("--origin %q is not an origin, such as https://client.example%s", o, asciiHint(o))
	}
	origin := scheme + "://" + host
	if userinfo || (rest != "" && rest != "/") {
		return "", fmt.Errorf("an origin is a scheme and a host, without the rest of %q: --origin %s", o, shellWord(origin))
	}
	return origin, nil
}

// refusals names each refused Host and Origin in serve's log once, with the
// flag that admits it, for the person who set up a tunnel or a page. Whatever
// reaches the listener, it names at most maxRefusals values of maxRefused
// bytes each.
type refusals struct {
	mu   sync.Mutex
	seen map[string]bool
}

const maxRefusals = 32
const maxRefused = 256

// note logs a refused header's value once. A value that could be the
// person's comes with the flag that would admit it, written as a shell takes
// it literally; whether it is theirs is for them to say, since anyone who
// reaches the listener chooses it.
func (rs *refusals) note(l *log.Logger, header, value string) {
	advice := ""
	switch {
	case len(value) > maxRefused:
		value = value[:maxRefused]
	case header == "Host":
		if host, ok := canonicalHost(value); ok {
			advice = "if it is your tunnel's host, start tmag serve with --host " + shellWord(host)
		}
	case header == "Origin":
		if origin, err := allowedOrigin(value); err == nil {
			advice = "if it is your client's page, start tmag serve with --origin " + shellWord(origin)
		}
	}
	rs.mu.Lock()
	key := header + " " + value
	fresh := !rs.seen[key] && len(rs.seen) < maxRefusals
	if fresh {
		if rs.seen == nil {
			rs.seen = map[string]bool{}
		}
		rs.seen[key] = true
	}
	last := fresh && len(rs.seen) == maxRefusals
	rs.mu.Unlock()
	if !fresh {
		return
	}
	line := fmt.Sprintf("refused %s %q", header, value)
	if advice != "" {
		line += ": " + advice + " (prefix + M-r takes its flags from @tmag-serve)"
	}
	l.Print(line)
	if last {
		l.Printf("refused %d hosts and origins; the next ones go unnamed", maxRefusals)
	}
}

// hostOK reports whether a request's Host names loopback or an allowed host.
func (s *Server) hostOK(host string) bool {
	h, ok := canonicalHost(host)
	if !ok {
		return false
	}
	if slices.Contains(s.cfg.Hosts, h) {
		return true
	}
	name := h
	if n, _, err := net.SplitHostPort(h); err == nil {
		name = n
	}
	name = strings.Trim(name, "[]")
	return name == "localhost" || net.ParseIP(name).IsLoopback()
}

// originOK reports whether a request has no Origin, or one that is the
// listener's own or an allowed origin.
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	origin, err := allowedOrigin(o)
	if err != nil {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if host, ok := canonicalHost(r.Host); ok && origin == scheme+"://"+strings.TrimSuffix(host, defaultPort[scheme]) {
		return true
	}
	return slices.Contains(s.cfg.Origins, origin)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Tmag-API-Schema", Schema)
	w.Header().Set("Vary", "Origin")
	if !s.hostOK(r.Host) {
		s.refused.note(s.cfg.Log, "Host", r.Host)
		fail(w, 403, "forbidden", "host is not allowed")
		return
	}
	if !s.originOK(r) {
		s.refused.note(s.cfg.Log, "Origin", r.Header.Get("Origin"))
		fail(w, 403, "forbidden", "origin is not allowed")
		return
	}
	if o := r.Header.Get("Origin"); o != "" {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Access-Control-Expose-Headers", "Tmag-API-Schema")
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/v2/pair" && r.Method == http.MethodPost {
		s.pair(w, r)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		unauthorized(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	grant, err := s.cfg.Auth.Authenticate(ctx, token)
	cancel()
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			unauthorized(w)
		} else {
			fail(w, 503, "authorization_unavailable", "authorization store is unavailable")
		}
		return
	}
	s.mux.ServeHTTP(w, r.WithContext(query.WithOwner(r.Context(), grant)))
}
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="tmag", error="invalid_token"`)
	fail(w, 401, "unauthorized", "credential is missing, expired or revoked")
}
func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": Schema, "error": map[string]string{"code": code, "message": message}})
}
func body(w http.ResponseWriter, r *http.Request, out any) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()

	typ, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if typ != "application/json" {
		fail(w, 415, "content_type", "expected application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		fail(w, 400, "usage", "invalid JSON request")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		fail(w, 400, "usage", "expected one JSON document")
		return false
	}
	return true
}
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !body(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	issued, err := s.cfg.Auth.Redeem(ctx, in.Code)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			fail(w, 403, "pairing", "pairing code is invalid, expired or already used")
		} else {
			fail(w, 503, "authorization_unavailable", "authorization store is unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": Schema, "credential": issued})
}
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	instance, _, err := s.cfg.Auth.List(r.Context())
	if err != nil {
		fail(w, 503, "authorization_unavailable", "authorization store is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": Schema, "state_schema": version.Schema, "version": s.cfg.Version, "instance": instance,
		"operations": []string{"list", "inspect", "watch", "read", "changes", "dirs", "request", "send", "interrupt", "answer", "start", "keys"}})
}
func (s *Server) relay(w http.ResponseWriter, r *http.Request, args []string, input io.Reader) int {
	ctx, cancel := context.WithTimeout(r.Context(), actTimeout)
	defer cancel()
	var out bytes.Buffer
	code := s.cfg.Run(ctx, args, input, &out, io.Discard)
	if !json.Valid(out.Bytes()) {
		fail(w, 502, "backend", "local command returned no JSON document")
		return code
	}
	status := http.StatusOK
	switch code {
	case 0:
	case 2:
		status = 400
	case 3:
		status = 409
	case 4:
		status = 504
	default:
		status = 422
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out.Bytes())
	return code
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.relay(w, r, []string{"list", "--json"}, nil)
}
func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		fail(w, 400, "usage", "target is required")
		return
	}
	s.relay(w, r, []string{"inspect", "--json", "--", target}, nil)
}
func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("target") == "" || q.Get("request") == "" {
		fail(w, 400, "usage", "target and request are required")
		return
	}
	s.relay(w, r, []string{"request", "--json", "--", q.Get("target"), q.Get("request")}, nil)
}
func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := []string{"read", "--json"}
	if q.Get("target") == "" {
		fail(w, 400, "usage", "target is required")
		return
	}
	last, lines, turns := q.Get("last"), q.Get("lines"), q.Get("turns")
	switch {
	case last == "1" && lines == "" && turns == "":
		args = append(args, "--last")
	case last == "" && turns == "" && count(lines, 2000) > 0:
		args = append(args, "--lines", strconv.Itoa(count(lines, 2000)))
	case last == "" && lines == "" && count(turns, transcript.MaxTurns) > 0:
		args = append(args, "--turns", strconv.Itoa(count(turns, transcript.MaxTurns)))
	default:
		fail(w, 400, "usage", fmt.Sprintf("read needs last=1, lines=1..2000 or turns=1..%d", transcript.MaxTurns))
		return
	}
	s.relay(w, r, append(args, "--", q.Get("target")), nil)
}

// changes relays what an agent's directory changed, or one file's patch.
func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("target") == "" {
		fail(w, 400, "usage", "target is required")
		return
	}
	args := []string{"changes", "--json"}
	if file := q.Get("file"); file != "" {
		args = append(args, "--file="+file)
	}
	s.relay(w, r, append(args, "--", q.Get("target")), nil)
}

// dirs relays where an agent may start, or a directory's subdirectories.
func (s *Server) dirs(w http.ResponseWriter, r *http.Request) {
	args := []string{"dirs", "--json"}
	if dir := r.URL.Query().Get("dir"); dir != "" {
		args = append(args, "--", dir)
	}
	s.relay(w, r, args, nil)
}

// count is the number s names from 1 to most, or 0.
func count(s string, most int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > most {
		return 0
	}
	return n
}

type action struct {
	Verb     string            `json:"verb"`
	Target   string            `json:"target"`
	Agent    string            `json:"agent,omitempty"`
	Template string            `json:"template,omitempty"`
	Dir      string            `json:"dir,omitempty"`
	Worktree string            `json:"worktree,omitempty"`
	Text     string            `json:"text,omitempty"`
	Screen   string            `json:"screen,omitempty"`
	Keys     []string          `json:"keys,omitempty"`
	Request  string            `json:"request,omitempty"`
	Decision string            `json:"decision,omitempty"`
	Message  string            `json:"message,omitempty"`
	Answers  map[string]string `json:"answers,omitempty"`
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	var a action
	if !body(w, r, &a) {
		return
	}
	if a.Target == "" && a.Verb != "start" {
		fail(w, 400, "usage", "target is required")
		return
	}
	args := []string{a.Verb, "--json"}
	var input io.Reader
	switch a.Verb {
	case "start":
		name, ok := startName(a)
		if !ok || a.Target != "" || !filepath.IsAbs(a.Dir) {
			fail(w, 400, "usage", "start needs an agent or a template, and an absolute dir")
			return
		}
		args = append(args, "--dir="+a.Dir)
		if a.Worktree != "" {
			args = append(args, "--worktree="+a.Worktree)
		}
		args = append(args, "--", name)
		if a.Text != "" {
			args = append(args, "-")
			input = strings.NewReader(a.Text)
		}
	case "send":
		args = append(args, "--", a.Target, "-")
		input = strings.NewReader(a.Text)
	case "interrupt":
		args = append(args, "--", a.Target)
	case "keys":
		if a.Screen == "" {
			fail(w, 400, "usage", "keys needs the screen they answer")
			return
		}
		args = append(args, "--screen="+a.Screen, "--text="+a.Text, "--", a.Target)
		args = append(args, a.Keys...)
	case "answer":
		if a.Request == "" || (a.Decision != "allow" && a.Decision != "deny") {
			fail(w, 400, "usage", "answer needs a request and allow or deny")
			return
		}
		if a.Message != "" {
			args = append(args, "--message="+a.Message)
		}
		if a.Answers != nil {
			b, _ := json.Marshal(a.Answers)
			args = append(args, "--answers="+string(b))
		}
		args = append(args, "--", a.Target, a.Request, a.Decision)
	default:
		fail(w, 400, "usage", "unknown control verb")
		return
	}
	// Admission is after the complete, validated body. A client cannot retain
	// authority by authenticating, stalling its upload, then acting after revoke.
	quick, stop := context.WithTimeout(r.Context(), 5*time.Second)
	err := s.cfg.Auth.Validate(quick, query.OwnerGrant(r.Context()))
	stop()
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			unauthorized(w)
		} else {
			fail(w, 503, "authorization_unavailable", "authorization store is unavailable")
		}
		return
	}
	// Once admitted, delivery finishes within its bound even if its client
	// disconnects or revokes its credential. Reads and subscriptions cancel.
	r = r.WithContext(context.WithoutCancel(r.Context()))
	code := s.relay(w, r, args, input)
	s.cfg.Log.Printf("credential %s: %s %q: exit %d", query.OwnerGrant(r.Context()).ID(), a.Verb, a.Target, code)
}

// startName is what a start action starts: one of the agents, or a
// template the person set, by name; never a command.
func startName(a action) (string, bool) {
	switch {
	case a.Agent != "" && a.Template == "":
		return a.Agent, slices.Contains(proc.Agents, a.Agent)
	case a.Template != "" && a.Agent == "":
		return a.Template, !slices.Contains(proc.Agents, a.Template) && !strings.HasPrefix(a.Template, "-")
	}
	return "", false
}

func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("since")
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID")
	}
	args := []string{"watch"}
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			fail(w, 400, "usage", "since must be a nonnegative sequence number")
			return
		}
		args = append(args, "--since", cursor)
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	frames := make(chan []byte)
	go func() { defer writer.Close(); s.cfg.Run(ctx, args, nil, writer, io.Discard) }()
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(reader)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			b := bytes.Clone(sc.Bytes())
			if !json.Valid(b) {
				continue
			}
			select {
			case frames <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	send := func(frame string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(w, frame); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(": watching\n\n") {
		return
	}
	check := time.NewTicker(time.Second)
	defer check.Stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			// Check the persisted store so revocation from another CLI process also
			// closes existing subscriptions. Authorization-store failures fail closed.
			quick, stop := context.WithTimeout(ctx, 5*time.Second)
			active := s.cfg.Auth.Active(quick, query.OwnerGrant(ctx))
			stop()
			if !active {
				return
			}
		case <-ping.C:
			if !send("event: ping\ndata: 1\n\n") {
				return
			}
		case b, ok := <-frames:
			if !ok {
				return
			}
			var d struct {
				Type  string `json:"type"`
				Seq   int64  `json:"seq"`
				Event struct {
					Seq int64 `json:"seq"`
				} `json:"event"`
			}
			_ = json.Unmarshal(b, &d)
			id := ""
			switch d.Type {
			case "snapshot":
				id = fmt.Sprintf("id: %d\n", d.Seq)
			case "event":
				id = fmt.Sprintf("id: %d\n", d.Event.Seq)
			}
			if !send(id + "event: doc\ndata: " + string(b) + "\n\n") {
				return
			}
		}
	}
}
