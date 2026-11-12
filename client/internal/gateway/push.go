package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/push"
)

// visibleFor is how long a page's visibility report counts. A page that
// stops reporting is presumed gone, so its device gets pushes again.
const visibleFor = time.Minute

// visibility remembers which device's page was visible and when it said so.
type visibility struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (v *visibility) set(id string, visible bool, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if visible {
		v.at[id] = now
	} else {
		delete(v.at, id)
	}
}

func (v *visibility) visible(id string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	at, ok := v.at[id]
	return ok && now.Sub(at) < visibleFor
}

func (v *visibility) keep(active map[string]bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for id := range v.at {
		if !active[id] {
			delete(v.at, id)
		}
	}
}

// alertJob is one notification waiting for the push worker, with what the
// current session must still show when it goes out.
type alertJob struct {
	machine string
	agent   string
	session string
	alert   Alert
	msg     push.Message
}

// alert turns a machine's Alert event into a push, when the person is away,
// the kind is chosen and the state still agrees.
func (s *Server) alert(machine string, e Event) {
	a := e.Alert
	if s.push == nil || a == nil || a.Present || !s.kinds[a.Kind] {
		return
	}
	state := s.states[machine]
	if state == nil {
		return
	}
	session, ok := state.session(e.Agent, e.Session)
	if !ok || !alertHolds(*a, session) {
		return
	}
	job := alertJob{machine: machine, agent: e.Agent, session: e.Session, alert: *a, msg: alertMessage(machine, session, *a)}
	select {
	case s.queue <- job:
	default:
		s.cfg.Log.Printf("push %s for %s/%s dropped: queue full", a.Kind, machine, e.Session)
	}
}

// alertHolds discards an alert the current session already contradicts.
func alertHolds(a Alert, s Session) bool {
	switch a.Kind {
	case "blocked":
		if a.Request == nil {
			return false
		}
		for _, q := range s.Requests {
			if q.ID == a.Request.ID {
				return true
			}
		}
		return false
	case "finished":
		return s.State == "idle" && (a.Turn == "" || s.Turn == a.Turn)
	}
	return false
}

// alertMessage words a notification like tmag notify does, with the machine's
// name where the host would be.
func alertMessage(machine string, s Session, a Alert) push.Message {
	title := s.Agent + " finished"
	switch a.Kind {
	case "blocked":
		title = s.Agent + " needs your approval"
		if a.Request != nil {
			switch a.Request.Kind {
			case "question":
				title = s.Agent + " has a question"
			case "plan":
				title = s.Agent + " has a plan for you"
			}
		}
	case "finished":
		if s.Kind == "error" {
			title = s.Agent + " stopped on an error"
		}
	}
	var parts []string
	tool := ""
	if a.Request != nil {
		tool = a.Request.Tool
	}
	if tool != "" {
		parts = append(parts, tool)
	}
	dir := ""
	if s.Cwd != "" {
		dir = filepath.Base(s.Cwd)
		parts = append(parts, dir)
	}
	if machine != "" {
		parts = append(parts, machine)
	}
	body := ""
	for i, p := range parts {
		if i > 0 {
			body += " · "
		}
		body += p
	}
	request := ""
	if a.Request != nil {
		request = a.Request.ID
	}
	route := "#/s/" + url.PathEscape(s.ID)
	if request != "" {
		route += "/r/" + url.PathEscape(request)
	}
	tag := sessionTag(machine, s)
	ttl := 3600
	urgency := "normal"
	if a.Kind == "blocked" {
		ttl, urgency = 86400, "high"
	}
	return push.Message{
		Title: title, Body: body, Tag: tag, URL: route, Kind: a.Kind,
		Machine: machine, Agent: s.Agent, Session: s.ID, Request: request, Tool: tool, Dir: dir,
		Urgency: urgency, TTL: ttl, Topic: tag,
	}
}

// sessionTag names a notification for one session, so a newer one replaces
// it. A topic is 32 lower-case hex characters: Apple's push service refuses
// the URL-safe base64 RFC 8030 allows.
func sessionTag(machine string, s Session) string {
	sum := sha256.Sum256([]byte(machine + "\x00" + s.Agent + "\x00" + s.ID))
	return hex.EncodeToString(sum[:16])
}

// pushLoop sends queued alerts, away from every follower.
func (s *Server) pushLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.queue:
			s.sendAlert(job)
		}
	}
}

// sendAlert sends one job to every subscribed device, each independently so
// a slow service holds up only itself.
func (s *Server) sendAlert(job alertJob) {
	subs, err := s.auth.Subscriptions(s.ctx)
	if err != nil {
		s.cfg.Log.Printf("push: read subscriptions: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, entry := range subs {
		id := entry.ID
		wg.Go(func() { s.sendDevice(job, id) })
	}
	wg.Wait()
}

// sendDevice checks the device and the alert again, right before its send.
func (s *Server) sendDevice(job alertJob, id string) {
	if s.vis.visible(id, time.Now()) {
		return
	}
	sub, ok, err := s.auth.Subscription(s.ctx, id)
	if err != nil {
		s.cfg.Log.Printf("push %s: read subscription: %v", job.msg.Kind, err)
		return
	}
	if !ok {
		return
	}
	state := s.states[job.machine]
	if state == nil {
		return
	}
	session, ok := state.session(job.agent, job.session)
	if !ok || !alertHolds(job.alert, session) {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	err = s.push.Send(ctx, push.Subscription{Endpoint: sub.Endpoint, P256DH: sub.P256DH, Auth: sub.Auth}, job.msg)
	switch {
	case errors.Is(err, push.ErrGone):
		if err := s.auth.UnsubscribeIf(s.ctx, id, sub); err != nil {
			s.cfg.Log.Printf("push %s: drop gone subscription: %v", job.msg.Kind, err)
		}
	case err != nil:
		s.cfg.Log.Printf("push %s to credential %s: %v", job.msg.Kind, id, err)
	}
}
