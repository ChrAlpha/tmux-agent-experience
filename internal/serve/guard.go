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
	case local(r):
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

// local reports whether a request comes straight from this machine: a
// loopback host name and no forwarding header.
func local(r *http.Request) bool { return loopback(hostName(r.Host)) && !forwarded(r) }

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
// that a phone gets it only from someone who can see that window. A code is
// twelve characters of Crockford's base32, about 60 bits, too many to guess
// through however fast one tries, so a wrong try costs nothing but itself and
// locks nobody out. A code pairs one device and lasts ten minutes; each new
// code is shown in the window.
type pairing struct {
	mu      sync.Mutex
	code    string // normalized: twelve symbols, no dashes
	expires time.Time
	now     func() time.Time
	show    func(code, why string)
}

const pairCodeLife = 10 * time.Minute

// crockford is Crockford's base32 alphabet: no I, L, O or U to misread.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newPairing(show func(code, why string)) *pairing {
	p := &pairing{now: time.Now, show: show}
	p.renew("")
	return p
}

// renew draws a new code and shows it, saying why the last one went.
func (p *pairing) renew(why string) {
	var b [12]byte
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(crockford))))
		if err != nil {
			panic(err) // crypto/rand does not fail on supported platforms
		}
		b[i] = crockford[n.Int64()]
	}
	p.code = string(b[:])
	p.expires = p.now().Add(pairCodeLife)
	if p.show != nil {
		p.show(grouped(p.code), why)
	}
}

// grouped writes a code as people read it: XXXX-XXXX-XXXX.
func grouped(code string) string { return code[:4] + "-" + code[4:8] + "-" + code[8:] }

// normalized reads a code as people type it: in either case, with dashes or
// spaces, O for 0 and I or L for 1.
func normalized(code string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(code) {
		switch c {
		case '-', ' ':
		case 'O':
			b.WriteRune('0')
		case 'I', 'L':
			b.WriteRune('1')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Code is the code that pairs a device now, as people read it.
func (p *pairing) Code() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now().After(p.expires) {
		p.renew("the last code expired")
	}
	return grouped(p.code)
}

// try reports whether code pairs a device, which asks through from. A code
// pairs one device: a new one replaces it once it has.
func (p *pairing) try(code, from string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now().After(p.expires) {
		p.renew("the last code expired")
		return false, errors.New("that code expired; tmag serve's window shows a new one")
	}
	if subtle.ConstantTimeCompare([]byte(normalized(code)), []byte(p.code)) != 1 {
		return false, errors.New("wrong code")
	}
	p.renew(fmt.Sprintf("a device paired through %q", from))
	return true, nil
}
