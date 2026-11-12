package gateway

import (
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
)

// authorizationHeader carries this device's credential on every API request.
// A header, unlike a cookie, belongs to the page's own origin: no other page,
// and no other port of the machine, gets it from the browser, and no other
// site can make the browser send it.
const authorizationHeader = "Authorization"

// forwardingHeaders are set by proxies and tunnels. A request that carries
// any of them did not come straight from a browser on this machine, whatever
// host name it names.
var forwardingHeaders = []string{
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	"Cf-Connecting-Ip", "Cf-Ray", "Cf-Access-Jwt-Assertion",
	"Tailscale-User-Login", "Tailscale-Headers-Info", "Tailscale-Funnel-Request",
}

var (
	errNotOwner   = errors.New("not the owner")
	errHost       = errors.New("unknown host name")
	errCredential = errors.New("pair this device: open the page and enter the code tmag-client shows")
)

// guard admits the owner alone. A request must come in a way serve knows,
// with that way's proof of the owner, and, for anything but the page itself,
// carry a credential this device paired with: the tunnels' proofs come with
// every request from anyone who can reach loopback and set headers, other
// users of the machine too, so only a credential, which one device redeemed,
// tells the owner's own browser.
type guard struct {
	owner     string
	tailscale []string
	access    []string
	keys      *accessKeys
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
