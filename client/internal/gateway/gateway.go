// Package gateway is tmag-client's gateway: it follows each machine once,
// keeps their sessions in memory, and serves one person's page from that
// state: the inbox, each session, and answers, prompts and interrupts, each
// one tmag verb whose own gate decides on its machine. It pushes alerts to
// paired devices and stores no content: replies, pane text and tool input go
// from a machine to the page that asked, and the state directory holds ids,
// turn ids, digests and keys.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/devices"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/webpush"
)

// Config is what the gateway serves, and how.
type Config struct {
	// Machines are the machines whose agents the page shows, in its order.
	Machines []machine.Machine
	// Public are the origins the gateway is reached at through a tunnel,
	// such as https://box.example.ts.net. Push needs one.
	Public []*url.URL
	// Devices are the paired devices.
	Devices *devices.Store
	// StateDir keeps the turns people read and the push keys.
	StateDir string
	// Push are the alert kinds pushed to devices: blocked, finished. Empty
	// turns push off.
	Push []string
	// PushServices are push services beside the browsers' own.
	PushServices []string
	// PushHTTP sends pushes; nil is webpush's own client.
	PushHTTP *http.Client
	// Version is tmag-client's, for the page.
	Version string
	// Log gets a line per action and per failure, never content.
	Log *log.Logger
}

// Gateway serves the page and its API.
type Gateway struct {
	cfg     Config
	world   *world
	byName  map[string]machine.Machine
	push    *webpush.Client // nil when push is off or has no public origin
	kinds   map[string]bool
	visible *visibility
	pairing *limiter
	jobs    chan pushJob
	mux     *http.ServeMux
	icons   map[string][]byte
	wg      sync.WaitGroup
}

// New checks cfg and returns the gateway; Run follows the machines.
func New(cfg Config) (*Gateway, error) {
	if len(cfg.Machines) == 0 {
		return nil, errors.New("no machine to follow")
	}
	if cfg.Devices == nil || cfg.StateDir == "" {
		return nil, errors.New("the gateway needs its devices and state directory")
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	g := &Gateway{cfg: cfg, byName: map[string]machine.Machine{}, kinds: map[string]bool{}, visible: newVisibility(), pairing: newLimiter(10, time.Minute), jobs: make(chan pushJob, 64)}
	var names []string
	for _, m := range cfg.Machines {
		if err := machine.CheckName(m.Name); err != nil {
			return nil, err
		}
		if _, dup := g.byName[m.Name]; dup {
			return nil, fmt.Errorf("machine %q is named twice", m.Name)
		}
		g.byName[m.Name] = m
		names = append(names, m.Name)
	}
	for _, u := range cfg.Public {
		if u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("public address %s is not an https origin such as https://box.example.ts.net", u)
		}
	}
	for _, k := range cfg.Push {
		if k != "blocked" && k != "finished" {
			return nil, fmt.Errorf("alert kind %q is neither blocked nor finished", k)
		}
		g.kinds[k] = true
	}
	seen, err := loadSeen(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("read turns: %w", err)
	}
	g.world = newWorld(names, seen)
	if len(g.kinds) > 0 && len(cfg.Public) > 0 {
		keys, err := webpush.LoadKeys(cfg.StateDir)
		if err != nil {
			return nil, fmt.Errorf("push keys: %w", err)
		}
		g.push = &webpush.Client{Keys: keys, Subject: origin(cfg.Public[0]), Extra: cfg.PushServices, HTTP: cfg.PushHTTP}
	}
	g.icons = icons()
	g.routes()
	return g, nil
}

// Run follows every machine and sends pushes until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	for _, m := range g.cfg.Machines {
		g.wg.Go(func() { g.follow(ctx, m) })
	}
	if g.push != nil {
		g.wg.Go(func() { g.pushLoop(ctx) })
	}
	g.wg.Go(func() { g.watchDevices(ctx) })
	g.wg.Wait()
}

// PairURL is the address that pairs a device with code: on the first public
// origin, or on addr when there is none.
func (g *Gateway) PairURL(addr, code string) string {
	base := "http://" + addr
	if len(g.cfg.Public) > 0 {
		base = origin(g.cfg.Public[0])
	}
	return base + "/#pair=" + code
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// ServeHTTP admits a request by its host and origin, then routes it.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	if !g.hostOK(r.Host) {
		fail(w, http.StatusForbidden, "host", fmt.Sprintf("%s is not an address of this gateway; start it with --public https://%s", r.Host, r.Host))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !g.originOK(r) {
		fail(w, http.StatusForbidden, "origin", "the request comes from another site")
		return
	}
	g.mux.ServeHTTP(w, r)
}

// hostOK admits loopback, as the gateway listens, and the public origins'
// hosts: a name that resolves here by other means is refused.
func (g *Gateway) hostOK(host string) bool {
	for _, u := range g.cfg.Public {
		if strings.EqualFold(host, u.Host) {
			return true
		}
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	ip := net.ParseIP(name)
	return strings.EqualFold(name, "localhost") || (ip != nil && ip.IsLoopback())
}

// originOK admits a request with no Origin, as tools send, or from this
// page.
func (g *Gateway) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host) && slices.Contains([]string{"http", "https"}, u.Scheme)
}

// visibility is when each device's page last said it was on screen.
type visibility struct {
	mu sync.Mutex
	at map[string]time.Time
}

// visibleFor is how long a page's word that it is on screen holds; a page
// says it again every twenty seconds.
const visibleFor = 45 * time.Second

func newVisibility() *visibility { return &visibility{at: map[string]time.Time{}} }

func (v *visibility) set(device string, on bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if on {
		v.at[device] = time.Now()
	} else {
		delete(v.at, device)
	}
}

func (v *visibility) on(device string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return time.Since(v.at[device]) < visibleFor
}

// limiter bounds failed pairings: n in each window, for everyone together.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	fails  []time.Time
}

func newLimiter(n int, window time.Duration) *limiter { return &limiter{n: n, window: window} }

func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-l.window)
	l.fails = slices.DeleteFunc(l.fails, func(t time.Time) bool { return t.Before(cut) })
	return len(l.fails) < l.n
}

func (l *limiter) failed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails = append(l.fails, time.Now())
}
