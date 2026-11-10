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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
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
	Origins []string // additional exact browser origins
	Version string
	Log     *log.Logger
}
type Server struct {
	cfg Config
	mux *http.ServeMux
}

func New(cfg Config) (*Server, error) {
	if cfg.Auth.Dir == "" || cfg.Run == nil {
		return nil, errors.New("serve needs an authorization store and a local runner")
	}
	for _, h := range cfg.Hosts {
		if h == "" || strings.ContainsAny(h, "/\\@#? \t\r\n*") {
			return nil, fmt.Errorf("invalid allowed host %q", h)
		}
	}
	for _, o := range cfg.Origins {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("invalid allowed origin %q", o)
		}
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v2/info", s.info)
	s.mux.HandleFunc("GET /v2/list", s.list)
	s.mux.HandleFunc("GET /v2/inspect", s.inspect)
	s.mux.HandleFunc("GET /v2/request", s.request)
	s.mux.HandleFunc("GET /v2/read", s.read)
	s.mux.HandleFunc("GET /v2/watch", s.watch)
	s.mux.HandleFunc("POST /v2/act", s.act)
	return s, nil
}
func same(a, b string) bool { return strings.EqualFold(a, b) }
func (s *Server) hostOK(host string) bool {
	for _, h := range s.cfg.Hosts {
		if same(host, h) {
			return true
		}
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	return same(name, "localhost") || net.ParseIP(name).IsLoopback()
}
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if same(u.Host, r.Host) && u.Scheme == scheme {
		return true
	}
	for _, allowed := range s.cfg.Origins {
		if o == allowed {
			return true
		}
	}
	return false
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Tmag-API-Schema", Schema)
	w.Header().Set("Vary", "Origin")
	if !s.hostOK(r.Host) || !s.originOK(r) {
		fail(w, 403, "forbidden", "host or origin is not allowed")
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
		"operations": []string{"list", "inspect", "watch", "read", "request", "send", "interrupt", "answer"}})
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
	if q.Get("last") == "1" && q.Get("lines") == "" {
		args = append(args, "--last")
	} else if n, err := strconv.Atoi(q.Get("lines")); err == nil && n > 0 && n <= 2000 && q.Get("last") == "" {
		args = append(args, "--lines", strconv.Itoa(n))
	} else {
		fail(w, 400, "usage", "read needs last=1 or lines=1..2000")
		return
	}
	s.relay(w, r, append(args, "--", q.Get("target")), nil)
}

type action struct {
	Verb     string            `json:"verb"`
	Target   string            `json:"target"`
	Text     string            `json:"text,omitempty"`
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
	if a.Target == "" {
		fail(w, 400, "usage", "target is required")
		return
	}
	args := []string{a.Verb, "--json"}
	var input io.Reader
	switch a.Verb {
	case "send":
		args = append(args, "--", a.Target, "-")
		input = strings.NewReader(a.Text)
	case "interrupt":
		args = append(args, "--", a.Target)
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
