package cli

import (
	"slices"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	for in, want := range map[string][]string{
		`--owner me@example.com --tailscale box.tail1234.ts.net`:               {"--owner", "me@example.com", "--tailscale", "box.tail1234.ts.net"},
		`--machine 'box=ssh box tmag' --machine "dev=docker exec -i dev tmag"`: {"--machine", "box=ssh box tmag", "--machine", "dev=docker exec -i dev tmag"},
		`--name it\'s  --addr=127.0.0.1:7777`:                                  {"--name", "it's", "--addr=127.0.0.1:7777"},
		`"a \"quoted\" \$word"`:                                                {`a "quoted" $word`},
		`''`:                                                                   {""},
		"":                                                                     nil,
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

func TestServeTunnelURL(t *testing.T) {
	f, err := parseServe([]string{"--tailscale", "box.tail1234.ts.net", "--owner", "me"})
	if err != nil || f.tunnelURL() != "https://box.tail1234.ts.net" {
		t.Fatalf("tunnel URL %q, %v", f.tunnelURL(), err)
	}
	if f, _ := parseServe(nil); f.tunnelURL() != "" {
		t.Fatalf("no tunnel, URL %q", f.tunnelURL())
	}
}
