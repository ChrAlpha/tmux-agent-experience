package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	for in, want := range map[string][]string{
		`--host box.example --origin https://client.example`:     {"--host", "box.example", "--origin", "https://client.example"},
		`--origin 'https://client.example' --host "box.example"`: {"--origin", "https://client.example", "--host", "box.example"},
		`--host it\'s  --addr=127.0.0.1:7777`:                    {"--host", "it's", "--addr=127.0.0.1:7777"},
		`"a \"quoted\" \$word"`:                                  {`a "quoted" $word`},
		`''`:                                                     {""},
		"":                                                       nil,
	} {
		got, err := splitArgs(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`'open`, `"open`} {
		if _, err := splitArgs(bad); err == nil {
			t.Errorf("splitArgs(%q) succeeded", bad)
		}
	}
}

// Serve says what holds its address, a tmag serve only when one answers as
// one, and refuses an address beyond loopback before it listens there.
func TestServeNamesWhatHoldsItsAddress(t *testing.T) {
	ctx := context.Background()
	tmag := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Tmag-API-Schema", "tmag/http/v2")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer tmag.Close()
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	redirect := httptest.NewServer(http.RedirectHandler(tmag.URL+"/v2/info", http.StatusFound))
	defer redirect.Close()
	port := func(s *httptest.Server) string { return strconv.Itoa(s.Listener.Addr().(*net.TCPAddr).Port) }
	for _, c := range []struct {
		addr string
		tmag bool
	}{{"localhost:" + port(tmag), true}, {other.Listener.Addr().String(), false}, {redirect.Listener.Addr().String(), false}} {
		ln, err := listenLoopback(ctx, c.addr)
		if err == nil {
			ln.Close()
			t.Fatalf("listened on %s, which is in use", c.addr)
		}
		var inUse addrInUse
		if !errors.As(err, &inUse) || inUse.tmag != c.tmag {
			t.Errorf("%s: %v, want in use by tmag serve %v", c.addr, err, c.tmag)
		}
		if c.tmag && err.Error() != "tmag serve already runs at "+tmag.URL {
			t.Errorf("%s: %v, want the numeric URL clients reach, %s", c.addr, err, tmag.URL)
		}
		if next("serve", "", err) == "" {
			t.Errorf("%v says nothing about what to do next", err)
		}
	}
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:7777"} {
		ln, err := listenLoopback(ctx, addr)
		if err == nil {
			ln.Close()
		}
		if !errors.As(err, new(notLoopback)) || next("serve", "", err) == "" {
			t.Errorf("%s: %v, want a refusal beyond loopback that says what next", addr, err)
		}
	}
	ln, err := listenLoopback(ctx, "localhost:0")
	if err != nil {
		t.Fatalf("localhost: %v", err)
	}
	ln.Close()
	for addr, want := range map[string]string{"127.0.0.1:http": "127.0.0.1:80", "[::1]:07777": "[::1]:7777", "[::ffff:127.0.0.1]:0": "127.0.0.1:0", "[::1%lo]:0": "[::1]:0"} {
		if got, err := loopbackAddr(ctx, addr); got != want || err != nil {
			t.Errorf("loopbackAddr(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
}

// Serve listens only where every address its host names is loopback, on an
// IPv4 one when there is one.
func TestLoopbackOnly(t *testing.T) {
	addrs := func(s ...string) []netip.Addr {
		var out []netip.Addr
		for _, a := range s {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}
	for _, c := range []struct {
		ips  []netip.Addr
		want string
	}{
		{addrs("::1", "127.0.0.1"), "127.0.0.1"},
		{addrs("::1"), "::1"},
		{addrs("::ffff:127.0.0.1"), "127.0.0.1"},
		{addrs("127.0.0.1", "192.168.1.20"), ""},
		{addrs("fe80::1%eth0"), ""},
		{nil, ""},
	} {
		got, ok := loopbackOnly(c.ips)
		if ok != (c.want != "") || (ok && got.String() != c.want) {
			t.Errorf("loopbackOnly(%v) = %v, %v; want %q", c.ips, got, ok, c.want)
		}
	}
}
