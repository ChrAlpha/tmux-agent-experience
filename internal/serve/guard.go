package serve

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
)

// tokenCookie keeps the local token in the browser once given in the URL.
const tokenCookie = "tmag_serve"

// forwardingHeaders are set by proxies and tunnels. A request that carries
// any of them did not come straight from a browser on this machine, whatever
// host name it names.
var forwardingHeaders = []string{
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	"Cf-Connecting-Ip", "Cf-Ray", "Cf-Access-Jwt-Assertion",
	"Tailscale-User-Login", "Tailscale-Headers-Info", "Tailscale-Funnel-Request",
}

var (
	errNoToken  = errors.New("open the address tmag serve printed, with its token")
	errNotOwner = errors.New("not the owner")
	errHost     = errors.New("unknown host name")
)

// guard admits the owner alone, by the proof their way in gives.
type guard struct {
	owner     string
	tailscale []string
	access    []string
	keys      *accessKeys
	token     string
}

// admit decides whether a request comes from the owner:
//
//   - straight from this machine (a loopback host name, no forwarding
//     header), with the local token: other users of the machine reach
//     loopback too;
//   - forwarded by Tailscale Serve under one of its host names, with the
//     owner's login, which tailscaled sets for tailnet users and removes from
//     whatever a client sends;
//   - forwarded by a Cloudflare tunnel under one of Access's host names, with
//     an Access token that verifies and names the owner.
//
// Any other host name is refused, which keeps DNS rebinding out.
func (g guard) admit(r *http.Request) error {
	host := hostName(r.Host)
	if loopback(host) && !forwarded(r) {
		if g.hasToken(r) {
			return nil
		}
		return errNoToken
	}
	switch {
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

// hasToken reports whether a local request carries the token, in its cookie
// or in the URL the person opened.
func (g guard) hasToken(r *http.Request) bool {
	if c, err := r.Cookie(tokenCookie); err == nil && g.isToken(c.Value) {
		return true
	}
	return g.isToken(r.URL.Query().Get("token"))
}

func (g guard) isToken(s string) bool {
	return s != "" && subtle.ConstantTimeCompare([]byte(s), []byte(g.token)) == 1
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
