package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/push"
)

func pushGateway(t *testing.T) *Server {
	t.Helper()
	bin, _ := fakeTmag(t, `echo '{"schema":"tmag/v2","type":"snapshot","seq":1,"sessions":[],"panes":[]}'; sleep 30`)
	s, _ := newServer(t, Config{Machines: []Machine{{Name: "here", Argv: []string{bin}}}, Owner: "me", Tailscale: []string{"box.ts.net"}, Push: []string{"blocked", "finished"}})
	return s
}

func subscribed(t *testing.T, s *Server, endpoint string) auth.Generated {
	t.Helper()
	issued := signIn(t, s)
	if err := s.auth.Subscribe(context.Background(), issued.ID, auth.Subscription{
		Endpoint: endpoint, P256DH: testP256(t), Auth: testAuth(),
	}); err != nil {
		t.Fatal(err)
	}
	return issued
}

func TestRevokedDeviceIsNotSent(t *testing.T) {
	s := pushGateway(t)
	sent := make(chan struct{}, 1)
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()
	issued := subscribed(t, s, pushSrv.URL+"/push")
	s.push.HTTP = pushSrv.Client()
	s.push.Endpoint = url.Parse
	s.states["here"].replace(1, []Session{{ID: "s1", Agent: "claude", State: "idle", Turn: "t1"}}, nil)
	if err := s.auth.Revoke(context.Background(), issued.ID); err != nil {
		t.Fatal(err)
	}
	s.sendDevice(alertJob{machine: "here", agent: "claude", session: "s1", alert: Alert{Kind: "finished", Turn: "t1"}, msg: push.Message{Kind: "finished"}}, issued.ID)
	select {
	case <-sent:
		t.Fatal("a revoked device got the push")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestQueuedAlertIsCheckedAgain(t *testing.T) {
	s := pushGateway(t)
	got := make(chan struct{}, 1)
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()
	subscribed(t, s, pushSrv.URL+"/push")
	s.push.HTTP = pushSrv.Client()
	s.push.Endpoint = url.Parse
	state := s.states["here"]
	blocked := alertJob{machine: "here", agent: "claude", session: "s1",
		alert: Alert{Kind: "blocked", Request: &AlertRequest{ID: "r1"}}, msg: push.Message{Kind: "blocked"}}
	state.replace(1, []Session{{ID: "s1", Agent: "claude", State: "blocked", Requests: []Request{{ID: "r1"}}}}, nil)
	// The request was answered while the alert waited.
	state.replace(2, []Session{{ID: "s1", Agent: "claude", State: "idle", Turn: "t1"}}, nil)
	s.sendAlert(blocked)
	select {
	case <-got:
		t.Fatal("a blocked alert whose request closed was sent")
	case <-time.After(300 * time.Millisecond):
	}
	finished := alertJob{machine: "here", agent: "claude", session: "s1",
		alert: Alert{Kind: "finished", Turn: "t1"}, msg: push.Message{Kind: "finished"}}
	state.replace(3, []Session{{ID: "s1", Agent: "claude", State: "idle", Turn: "t1"}}, nil)
	// The session started another turn while the alert waited.
	state.replace(4, []Session{{ID: "s1", Agent: "claude", State: "idle", Turn: "t2"}}, nil)
	s.sendAlert(finished)
	select {
	case <-got:
		t.Fatal("a finished alert for an older turn was sent")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPushSendsDevicesIndependently(t *testing.T) {
	s := pushGateway(t)
	slowStarted := make(chan struct{})
	release := make(chan struct{})
	fast := make(chan struct{})
	var onceSlow, onceFast sync.Once
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			onceSlow.Do(func() { close(slowStarted) })
			<-release
		case "/fast":
			onceFast.Do(func() { close(fast) })
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()
	subscribed(t, s, pushSrv.URL+"/slow")
	subscribed(t, s, pushSrv.URL+"/fast")
	s.push.HTTP = pushSrv.Client()
	s.push.Endpoint = url.Parse
	s.states["here"].replace(1, []Session{{ID: "s1", Agent: "claude", State: "idle", Turn: "t1"}}, nil)
	job := alertJob{machine: "here", agent: "claude", session: "s1", alert: Alert{Kind: "finished", Turn: "t1"}, msg: push.Message{Kind: "finished"}}
	done := make(chan struct{})
	go func() {
		s.sendAlert(job)
		close(done)
	}()
	select {
	case <-slowStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow device was not reached")
	}
	select {
	case <-fast:
	case <-time.After(2 * time.Second):
		t.Fatal("a slow device held up another device")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the push job did not finish")
	}
}
