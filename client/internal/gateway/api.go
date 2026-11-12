package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/devices"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/qr"
)

// Bounds of the API.
const (
	actTimeout  = 2 * time.Minute  // a verb runs to its end, even when the page goes
	readTimeout = 30 * time.Second // a read ends with the page that asked
	maxBody     = 1 << 20
	pingEvery   = 15 * time.Second
	screenLines = 2000
)

func (g *Gateway) routes() {
	mux := http.NewServeMux()
	for _, name := range []string{"/{$}", "/app.js", "/app.css", "/sw.js", "/manifest.webmanifest", "/icon.svg", "/icon-180.png", "/icon-512.png"} {
		mux.HandleFunc("GET "+name, g.asset)
	}
	mux.HandleFunc("POST /api/pair", g.pair)
	mux.HandleFunc("GET /api/events", g.authed(g.events))
	mux.HandleFunc("GET /api/reply", g.authed(g.reply))
	mux.HandleFunc("GET /api/screen", g.authed(g.screen))
	mux.HandleFunc("GET /api/request", g.authed(g.request))
	mux.HandleFunc("POST /api/send", g.authed(g.send))
	mux.HandleFunc("POST /api/interrupt", g.authed(g.interrupt))
	mux.HandleFunc("POST /api/answer", g.authed(g.answer))
	mux.HandleFunc("POST /api/read", g.authed(g.markRead))
	mux.HandleFunc("POST /api/presence", g.authed(g.presence))
	mux.HandleFunc("GET /api/push", g.authed(g.pushInfo))
	mux.HandleFunc("PUT /api/push", g.authed(g.subscribe))
	mux.HandleFunc("DELETE /api/push", g.authed(g.unsubscribe))
	mux.HandleFunc("GET /api/devices", g.authed(g.listDevices))
	mux.HandleFunc("POST /api/devices", g.authed(g.invite))
	mux.HandleFunc("DELETE /api/devices/{id}", g.authed(g.revoke))
	g.mux = mux
}

// authed admits a request whose bearer credential proves a paired device.
func (g *Gateway) authed(next func(http.ResponseWriter, *http.Request, devices.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			unauthorized(w)
			return
		}
		d, err := g.cfg.Devices.Authenticate(token)
		switch {
		case errors.Is(err, devices.ErrUnauthorized):
			unauthorized(w)
			return
		case err != nil:
			g.cfg.Log.Printf("devices: %v", err)
			fail(w, http.StatusServiceUnavailable, "devices", "the gateway cannot read its devices")
			return
		}
		next(w, r, d)
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="tmag-client"`)
	fail(w, http.StatusUnauthorized, "unauthorized", "this device is not paired, or no longer")
}

func fail(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body into v, refusing anything else.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if t, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); t != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "usage", "send JSON")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "usage", "the body is not the JSON this takes: "+err.Error())
		return false
	}
	return true
}

func (g *Gateway) pair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code  string `json:"code"`
		Label string `json:"label"` // what the device calls itself
	}
	if !decode(w, r, &in) {
		return
	}
	if !g.pairing.allow() {
		fail(w, http.StatusTooManyRequests, "pairing", "too many wrong codes; wait a minute")
		return
	}
	d, token, err := g.cfg.Devices.Redeem(in.Code, in.Label)
	if errors.Is(err, devices.ErrInvitation) {
		g.pairing.failed()
		fail(w, http.StatusForbidden, "pairing", "that code is wrong, expired or used; ask for a new one with tmag-client auth pair")
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "devices", err.Error())
		return
	}
	g.cfg.Log.Printf("paired %s (%s)", d.Label, d.ID)
	reply(w, http.StatusOK, map[string]any{"token": token, "device": map[string]any{"id": d.ID, "label": d.Label, "created": d.Created}})
}

// events streams the page everything, then each change: machine, session,
// gone, alert and devices frames, and a comment every pingEvery.
func (g *Gateway) events(w http.ResponseWriter, r *http.Request, d devices.Device) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	s := newStream(d.ID)
	me := map[string]any{"id": d.ID, "label": d.Label, "created": d.Created, "push": d.Push != nil}
	g.world.join(s, map[string]any{"version": g.cfg.Version, "device": me, "machines": len(g.cfg.Machines)})
	defer g.world.leave(s)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	write := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ping.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
		case b := <-s.frames:
			if !write(b) {
				return
			}
		}
	}
}

// target is the session a request names by its key, and its machine.
func (g *Gateway) target(w http.ResponseWriter, k string) (View, machine.Machine, bool) {
	v, _, ok := g.world.session(k)
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "no such session; it may have ended")
		return View{}, machine.Machine{}, false
	}
	return v, g.byName[v.Machine], true
}

// call runs a verb and answers with its document, or its refusal.
func (g *Gateway) call(w http.ResponseWriter, ctx context.Context, m machine.Machine, op machine.Op) (json.RawMessage, bool) {
	doc, err := m.Call(ctx, op)
	if err == nil {
		return doc, true
	}
	var e *machine.Error
	if !errors.As(err, &e) {
		e = &machine.Error{Code: "error", Message: err.Error()}
		if errors.Is(err, context.DeadlineExceeded) {
			e.Code, e.Message = "timeout", "the machine took too long"
		}
	}
	status := http.StatusUnprocessableEntity
	switch {
	case strings.HasPrefix(e.Code, "refused:"):
		status = http.StatusConflict
	case e.Code == "not_found" || e.Code == "ended" || e.Code == "no_reply":
		status = http.StatusNotFound
	case e.Code == "timeout":
		status = http.StatusGatewayTimeout
	case e.Code == machine.Unreachable:
		status = http.StatusBadGateway
	case e.Code == "usage":
		status = http.StatusBadRequest
	}
	reply(w, status, map[string]any{"error": e})
	return nil, false
}

func (g *Gateway) reply(w http.ResponseWriter, r *http.Request, _ devices.Device) {
	v, m, ok := g.target(w, r.URL.Query().Get("key"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	if doc, ok := g.call(w, ctx, m, machine.Op{Verb: "read", Target: v.ID, Last: true}); ok {
		var d struct {
			Turn string `json:"turn"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(doc, &d)
		reply(w, http.StatusOK, d)
	}
}

// screen reads the recent text of a pane that shows the session: its main
// view if one shows it.
func (g *Gateway) screen(w http.ResponseWriter, r *http.Request, _ devices.Device) {
	v, m, ok := g.target(w, r.URL.Query().Get("key"))
	if !ok {
		return
	}
	lines, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || lines < 1 || lines > screenLines {
		fail(w, http.StatusBadRequest, "usage", fmt.Sprintf("lines is 1 to %d", screenLines))
		return
	}
	if len(v.Panes) == 0 {
		fail(w, http.StatusConflict, "no_pane", "no pane shows this session")
		return
	}
	p := v.Panes[0]
	if i := slices.IndexFunc(v.Panes, func(p PaneView) bool { return p.View == "main" }); i >= 0 {
		p = v.Panes[i]
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	if doc, ok := g.call(w, ctx, m, machine.Op{Verb: "read", Target: p.Server + ":" + p.ID, Lines: lines}); ok {
		var d struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(doc, &d)
		reply(w, http.StatusOK, map[string]string{"text": d.Text, "target": p.Target})
	}
}

func (g *Gateway) request(w http.ResponseWriter, r *http.Request, _ devices.Device) {
	v, m, ok := g.target(w, r.URL.Query().Get("key"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	if doc, ok := g.call(w, ctx, m, machine.Op{Verb: "request", Target: v.ID, Request: r.URL.Query().Get("request")}); ok {
		var d struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(doc, &d)
		reply(w, http.StatusOK, d)
	}
}

// act runs a control verb to its end, even when the page that asked goes
// away: a lost answer is an unknown outcome, which the page reads back from
// the session's state.
func (g *Gateway) act(w http.ResponseWriter, r *http.Request, d devices.Device, k string, op machine.Op) {
	v, m, ok := g.target(w, k)
	if !ok {
		return
	}
	op.Target = v.ID
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), actTimeout)
	defer cancel()
	doc, ok := g.call(w, ctx, m, op)
	outcome := "ok"
	if !ok {
		outcome = "refused or failed"
	}
	g.cfg.Log.Printf("%s %s from %s (%s): %s", op.Verb, k, d.Label, d.ID, outcome)
	if ok {
		var dl struct {
			Via string `json:"via"`
		}
		_ = json.Unmarshal(doc, &dl)
		reply(w, http.StatusOK, map[string]string{"via": dl.Via})
	}
}

func (g *Gateway) send(w http.ResponseWriter, r *http.Request, d devices.Device) {
	var in struct {
		Key  string `json:"key"`
		Text string `json:"text"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		fail(w, http.StatusBadRequest, "usage", "the prompt is empty")
		return
	}
	g.act(w, r, d, in.Key, machine.Op{Verb: "send", Text: in.Text})
}

func (g *Gateway) interrupt(w http.ResponseWriter, r *http.Request, d devices.Device) {
	var in struct {
		Key string `json:"key"`
	}
	if decode(w, r, &in) {
		g.act(w, r, d, in.Key, machine.Op{Verb: "interrupt"})
	}
}

func (g *Gateway) answer(w http.ResponseWriter, r *http.Request, d devices.Device) {
	var in struct {
		Key      string            `json:"key"`
		Request  string            `json:"request"`
		Decision string            `json:"decision"`
		Message  string            `json:"message"`
		Answers  map[string]string `json:"answers"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Request == "" || (in.Decision != "allow" && in.Decision != "deny") {
		fail(w, http.StatusBadRequest, "usage", "an answer names a request and allow or deny")
		return
	}
	g.act(w, r, d, in.Key, machine.Op{Verb: "answer", Request: in.Request, Decision: in.Decision, Message: in.Message, Answers: in.Answers})
}

func (g *Gateway) markRead(w http.ResponseWriter, r *http.Request, _ devices.Device) {
	var in struct {
		Key  string `json:"key"`
		Turn string `json:"turn"`
	}
	if !decode(w, r, &in) {
		return
	}
	switch err := g.world.read(in.Key, in.Turn); {
	case errors.Is(err, errStale):
		fail(w, http.StatusConflict, "stale", err.Error())
	case err != nil:
		g.cfg.Log.Printf("read turns: %v", err)
		fail(w, http.StatusInternalServerError, "error", "the gateway could not keep the turn read")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (g *Gateway) presence(w http.ResponseWriter, r *http.Request, d devices.Device) {
	var in struct {
		Visible bool `json:"visible"`
	}
	if decode(w, r, &in) {
		g.visible.set(d.ID, in.Visible)
		w.WriteHeader(http.StatusNoContent)
	}
}

// pushInfo tells the page whether it can turn notifications on, and with
// which key.
func (g *Gateway) pushInfo(w http.ResponseWriter, _ *http.Request, d devices.Device) {
	out := map[string]any{"subscribed": d.Push != nil}
	switch {
	case len(g.kinds) == 0:
		out["reason"] = "off"
	case g.push == nil:
		out["reason"] = "no_public"
	default:
		out["key"] = g.push.Keys.Public()
	}
	reply(w, http.StatusOK, out)
}

func (g *Gateway) subscribe(w http.ResponseWriter, r *http.Request, d devices.Device) {
	var in struct {
		Endpoint       string            `json:"endpoint"`
		ExpirationTime *int64            `json:"expirationTime"`
		Keys           map[string]string `json:"keys"`
	}
	if !decode(w, r, &in) {
		return
	}
	if g.push == nil {
		fail(w, http.StatusConflict, "no_push", "this gateway pushes nothing")
		return
	}
	if err := g.push.Allowed(in.Endpoint); err != nil {
		fail(w, http.StatusBadRequest, "push_service", err.Error())
		return
	}
	if in.Keys["p256dh"] == "" || in.Keys["auth"] == "" {
		fail(w, http.StatusBadRequest, "usage", "a subscription carries its p256dh and auth keys")
		return
	}
	if err := g.cfg.Devices.Subscribe(d.ID, subscription(in.Endpoint, in.Keys)); err != nil {
		fail(w, http.StatusServiceUnavailable, "devices", err.Error())
		return
	}
	g.cfg.Log.Printf("%s (%s) turned notifications on", d.Label, d.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) unsubscribe(w http.ResponseWriter, _ *http.Request, d devices.Device) {
	if err := g.cfg.Devices.Unsubscribe(d.ID, ""); err != nil {
		fail(w, http.StatusServiceUnavailable, "devices", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) listDevices(w http.ResponseWriter, _ *http.Request, d devices.Device) {
	list, err := g.cfg.Devices.Devices()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "devices", err.Error())
		return
	}
	type entry struct {
		ID      string     `json:"id"`
		Label   string     `json:"label"`
		Created time.Time  `json:"created"`
		Expires *time.Time `json:"expires,omitempty"`
		Push    bool       `json:"push"`
		This    bool       `json:"this"`
	}
	out := []entry{}
	for _, x := range list {
		out = append(out, entry{x.ID, x.Label, x.Created, x.Expires, x.Push != nil, x.ID == d.ID})
	}
	reply(w, http.StatusOK, map[string]any{"devices": out})
}

// invite pairs another device from this one: a code, and the address that
// carries it as a QR code.
func (g *Gateway) invite(w http.ResponseWriter, r *http.Request, d devices.Device) {
	var in struct {
		Label string `json:"label"`
	}
	if !decode(w, r, &in) {
		return
	}
	inv, err := g.cfg.Devices.Invite(in.Label, 0)
	if err != nil {
		fail(w, http.StatusBadRequest, "usage", err.Error())
		return
	}
	link := g.PairURL(r.Host, inv.Code)
	out := map[string]any{"code": inv.Code, "expires": inv.Expires, "url": link}
	if code, err := qr.Encode(link); err == nil {
		out["qr"] = code.SVG()
	}
	g.cfg.Log.Printf("%s (%s) invited %s", d.Label, d.ID, inv.Label)
	reply(w, http.StatusOK, out)
}

func (g *Gateway) revoke(w http.ResponseWriter, r *http.Request, d devices.Device) {
	id := r.PathValue("id")
	switch err := g.cfg.Devices.Revoke(id); {
	case errors.Is(err, devices.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", "no such device")
		return
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "devices", err.Error())
		return
	}
	for _, s := range g.world.streamsOf(id) {
		s.close()
	}
	g.cfg.Log.Printf("%s (%s) revoked %s", d.Label, d.ID, id)
	w.WriteHeader(http.StatusNoContent)
}
