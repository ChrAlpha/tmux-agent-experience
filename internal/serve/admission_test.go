package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/admission"
)

func TestWatchAdmissionPrecedesAuthenticationAndRunner(t *testing.T) {
	runs := 0
	s, _, _ := fixture(t, func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { runs++; return 0 })
	admissions := 0
	s.cfg.Admit = func(context.Context, string, admission.Class) (*admission.Lease, error) {
		admissions++
		return nil, admission.ErrLimit
	}
	// No bearer: capacity rejection happens before authorization or pipe creation.
	w := call(s, "GET", "/v1/watch", "", "", "", "")
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" || admissions != 1 || runs != 0 {
		t.Fatalf("status=%d admissions=%d runs=%d", w.Code, admissions, runs)
	}
	w = call(s, "GET", "/v1/watch?since=bad", "", "", "", "")
	if w.Code != 400 || admissions != 1 {
		t.Fatal("invalid syntax consumed capacity", w.Code, admissions)
	}
	s.cfg.Admit = func(context.Context, string, admission.Class) (*admission.Lease, error) {
		return nil, admission.ErrUnavailable
	}
	if w := call(s, "GET", "/v1/watch", "", "", "", ""); w.Code != 503 {
		t.Fatalf("admission failure status %d", w.Code)
	}
}

func TestWatchJoinsRunnerBeforeReleasingBorrowedLease(t *testing.T) {
	entered := make(chan struct{})
	draining := make(chan struct{})
	release := make(chan struct{})
	var root string
	s, _, credential := fixture(t, func(ctx context.Context, _ []string, _ io.Reader, out, _ io.Writer) int {
		if !admission.Borrow(ctx, root, admission.Watch) {
			return 1
		}
		close(entered)
		_, _ = io.WriteString(out, "{\"type\":\"panes\",\"panes\":[]}\n")
		<-ctx.Done()
		close(draining)
		<-release
		return 0
	})
	root = s.cfg.StateDir
	var lease *admission.Lease
	s.cfg.Admit = func(ctx context.Context, root string, class admission.Class) (*admission.Lease, error) {
		var err error
		lease, err = admission.Acquire(ctx, root, class)
		return lease, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "http://localhost/v1/watch", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+credential.Token)
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("runner never entered with borrowed lease")
	}
	cancel()
	select {
	case <-draining:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("runner not canceled")
	}
	if _, err := lease.File().Stat(); err != nil {
		close(release)
		t.Fatal("released during drain", err)
	}
	select {
	case <-done:
		close(release)
		t.Fatal("handler returned before runner finished")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not join runner/scanner")
	}
	if _, err := lease.File().Stat(); err == nil {
		t.Fatal("lease leaked after shutdown")
	}
}
