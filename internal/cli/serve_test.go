package cli

import (
	"slices"
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
