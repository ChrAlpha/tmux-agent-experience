package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/devices"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/webpush"
)

// Notice is an alert as pages and notifications show it: what happened,
// where, and the route that opens it. It holds names and ids only.
type Notice struct {
	Kind    string `json:"kind"` // blocked or finished
	Key     string `json:"key"`
	Machine string `json:"machine"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Request string `json:"request,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	// Tag names the session, so a newer notification replaces an older one.
	Tag string `json:"tag"`
	URL string `json:"url"`
}

// alertFresh bounds how old an alert may be and still be pushed: a watch
// that resumes after a break replays the alerts it missed, which the page
// shows without a notification.
const alertFresh = 10 * time.Minute

// judge turns an Alert event into a notice, unless the session has moved on:
// a blocked alert whose request closed, a finished alert whose turn is no
// longer the one it is idle on, or has been read.
func (w *world) judge(name string, e *machine.Event) (Notice, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := key(name, e.Agent, e.Session)
	s, ok := w.machines[name].sessions[k]
	if !ok {
		return Notice{}, false
	}
	a := e.Alert
	n := Notice{Kind: a.Kind, Key: k, Machine: name, Agent: s.Agent, Session: s.ID, Tag: tag(k), URL: "/#/s/" + k}
	if s.Cwd != "" {
		n.Dir = filepath.Base(s.Cwd)
	}
	switch a.Kind {
	case "blocked":
		if s.State != "blocked" {
			return Notice{}, false
		}
		kind := s.Kind
		if a.Request != nil {
			i := slices.IndexFunc(s.Requests, func(r machine.Request) bool { return r.ID == a.Request.ID })
			if i < 0 {
				return Notice{}, false
			}
			n.Request, n.Tool, kind = a.Request.ID, a.Request.Tool, a.Request.Kind
		}
		switch kind {
		case "question":
			n.Title = s.Agent + " has a question"
		case "plan":
			n.Title = s.Agent + " has a plan for you"
		default:
			n.Title = s.Agent + " needs your approval"
		}
	case "finished":
		if s.State != "idle" || s.Turn == "" || s.Turn != a.Turn || w.seen.turn(k) == a.Turn {
			return Notice{}, false
		}
		n.Title = s.Agent + " finished"
		if s.Kind == "error" {
			n.Title = s.Agent + " stopped on an error"
		}
	default:
		return Notice{}, false
	}
	var parts []string
	for _, p := range []string{n.Tool, n.Dir, name} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	n.Body = strings.Join(parts, " · ")
	return n, true
}

// tag is a session's notification tag and push topic: 32 hexadecimal
// characters, which every push service takes as a topic.
func tag(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:16])
}

// alert tells every page, then pushes to each subscribed device whose page
// is not on screen, when the person was not at a terminal that shows the
// session.
func (g *Gateway) alert(name string, e *machine.Event) {
	n, ok := g.world.judge(name, e)
	if !ok {
		return
	}
	g.world.tell("alert", n)
	if g.push == nil || !g.kinds[n.Kind] || e.Alert.Present || time.Since(e.Time) > alertFresh {
		return
	}
	list, err := g.cfg.Devices.Devices()
	if err != nil {
		g.cfg.Log.Printf("push: devices: %v", err)
		return
	}
	for _, d := range list {
		if d.Push == nil || g.visible.on(d.ID) {
			continue
		}
		select {
		case g.jobs <- pushJob{device: d.ID, label: d.Label, endpoint: d.Push.Endpoint, notice: n}:
		default:
			g.cfg.Log.Printf("push to %s (%s): too many waiting, dropped", d.Label, d.ID)
		}
	}
}

type pushJob struct {
	device, label, endpoint string
	notice                  Notice
}

// pushParallel bounds the pushes in flight; each is bounded in time.
const (
	pushParallel = 4
	pushTimeout  = 20 * time.Second
)

func (g *Gateway) pushLoop(ctx context.Context) {
	slots := make(chan struct{}, pushParallel)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-g.jobs:
			slots <- struct{}{}
			g.wg.Go(func() {
				defer func() { <-slots }()
				g.deliver(ctx, job)
			})
		}
	}
}

// deliver pushes one notice to one device, if it is still paired and still
// subscribed where the job was made for.
func (g *Gateway) deliver(ctx context.Context, job pushJob) {
	d, ok, err := g.cfg.Devices.Device(job.device)
	if err != nil || !ok || d.Push == nil || d.Push.Endpoint != job.endpoint {
		return
	}
	payload, _ := json.Marshal(job.notice)
	m := webpush.Message{Payload: payload, TTL: time.Hour, Urgency: "normal", Topic: job.notice.Tag}
	if job.notice.Kind == "blocked" {
		m.Urgency = "high"
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	err = g.push.Send(ctx, webpush.Subscription{Endpoint: d.Push.Endpoint, P256DH: d.Push.P256DH, Auth: d.Push.Auth}, m)
	switch {
	case err == nil:
		g.cfg.Log.Printf("push to %s (%s): %s %s", job.label, job.device, job.notice.Kind, job.notice.Key)
	case errors.Is(err, webpush.ErrGone):
		g.cfg.Log.Printf("push to %s (%s): the subscription is gone; dropped it", job.label, job.device)
		_ = g.cfg.Devices.Unsubscribe(job.device, job.endpoint)
	default:
		g.cfg.Log.Printf("push to %s (%s): %v", job.label, job.device, err)
	}
}

// subscription reads a device's subscription as the page sends it, the
// browser's PushSubscription.toJSON().
func subscription(endpoint string, keys map[string]string) devices.Subscription {
	return devices.Subscription{Endpoint: endpoint, P256DH: keys["p256dh"], Auth: keys["auth"]}
}
