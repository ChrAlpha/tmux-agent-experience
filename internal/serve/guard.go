package serve

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// tokenHeader carries the token on every API request. A header, unlike a
// cookie, belongs to the page's own origin: no other page, and no other port
// of the machine, gets it from the browser, and no other site can make the
// browser send it.
const tokenHeader = "X-Tmag-Token"

// forwardingHeaders are set by proxies and tunnels. A request that carries
// any of them did not come straight from a browser on this machine, whatever
// host name it names.
var forwardingHeaders = []string{
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	"Cf-Connecting-Ip", "Cf-Ray", "Cf-Access-Jwt-Assertion",
	"Tailscale-User-Login", "Tailscale-Headers-Info", "Tailscale-Funnel-Request",
}

var (
	errNotOwner = errors.New("not the owner")
	errHost     = errors.New("unknown host name")
	errToken    = errors.New("pair this device: open the page and enter the code tmag serve shows")
)

// guard admits the owner alone. A request must come in a way serve knows,
// with that way's proof of the owner, and, for anything but the page itself,
// carry the token: the tunnels' proofs come with every request from anyone
// who can reach loopback and set headers, other users of the machine too,
// so only the token, which the person took from tmag serve's window, tells
// the owner's own browser.
type guard struct {
	owner     string
	tailscale []string
	access    []string
	keys      *accessKeys
	token     string
}

// entry checks the way a request came in:
//
//   - straight from this machine: a loopback host name and no forwarding
//     header;
//   - forwarded by Tailscale Serve under one of its host names, with the
//     owner's login, which tailscaled sets for tailnet users and removes from
//     whatever a client sends;
//   - forwarded by a Cloudflare tunnel under one of Access's host names, with
//     an Access token that verifies and names the owner.
//
// Any other host name is refused, which keeps DNS rebinding out.
func (g guard) entry(r *http.Request) error {
	host := hostName(r.Host)
	switch {
	case loopback(host) && !forwarded(r):
		return nil
	case slices.Contains(g.tailscale, host):
		if who := r.Header.Get("Tailscale-User-Login"); who != "" && strings.EqualFold(who, g.owner) {
			return nil
		}
		return errNotOwner
	case slices.Contains(g.access, host):
		email, err := g.keys.verify(r.Context(), r.Header.Get("Cf-Access-Jwt-Assertion"))
		if err == nil && strings.EqualFold(email, g.owner) {
			return nil
		}
		return errNotOwner
	}
	return errHost
}

// hasToken reports whether a request carries the token.
func (g guard) hasToken(r *http.Request) bool {
	t := r.Header.Get(tokenHeader)
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(g.token)) == 1
}

func forwarded(r *http.Request) bool {
	return slices.ContainsFunc(forwardingHeaders, func(h string) bool { return r.Header.Get(h) != "" })
}

// hostName is a request's host name, without its port, in lower case.
func hostName(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
	}
	return strings.ToLower(host)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Pairing hands a device the token for a code from tmag serve's window: so
// that a phone gets it only from someone who can see that window. A code
// pairs one device, lasts ten minutes, and gives way to a new one after five
// wrong tries, each new code shown in the window.
type pairing struct {
	mu       sync.Mutex
	code     string
	expires  time.Time
	attempts int
	now      func() time.Time
	show     func(code, why string)
}

const (
	pairCodeLife = 10 * time.Minute
	pairAttempts = 5
)

func newPairing(show func(code, why string)) *pairing {
	p := &pairing{now: time.Now, show: show}
	p.renew("")
	return p
}

// renew draws a new code and shows it, saying why the last one went.
func (p *pairing) renew(why string) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	p.code = fmt.Sprintf("%06d", n.Int64())
	p.expires = p.now().Add(pairCodeLife)
	p.attempts = 0
	if p.show != nil {
		p.show(p.code, why)
	}
}

// Code is the code that pairs a device now.
func (p *pairing) Code() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now().After(p.expires) {
		p.renew("the last code expired")
	}
	return p.code
}

// try reports whether code pairs a device, which asks through from; it uses
// up the code either way it ends: paired, expired, or tried too often.
func (p *pairing) try(code, from string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.now().After(p.expires):
		p.renew("the last code expired")
		return false, errors.New("that code expired; tmag serve's window shows a new one")
	case subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) == 1:
		p.renew(fmt.Sprintf("a device paired through %q", from))
		return true, nil
	}
	if p.attempts++; p.attempts >= pairAttempts {
		p.renew(fmt.Sprintf("%d wrong codes were tried", pairAttempts))
		return false, errors.New("too many wrong codes; tmag serve's window shows a new one")
	}
	return false, errors.New("wrong code")
}
