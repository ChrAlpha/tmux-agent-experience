package tmux

import "testing"

func TestUnquoteOption(t *testing.T) {
	for encoded, want := range map[string]string{
		`"printf \"%s\\n\" \"\$TMAG_ALERT\""`: `printf "%s\n" "$TMAG_ALERT"`,
		`"\\\$literal \\033\nnext\tline"`:     "\\$literal \\033\nnext\tline",
		`"\033[0m"`:                           "\x1b[0m",
		`'"quoted"'`:                          `"quoted"`,
		`\~`:                                  "~",
		`''`:                                  "",
		`on`:                                  "on",
	} {
		got, err := unquoteOption(encoded)
		if err != nil || got != want {
			t.Errorf("unquoteOption(%q) = %q, %v; want %q", encoded, got, err, want)
		}
	}
}

func TestPrintedDollarsKeepIntentionalEscapes(t *testing.T) {
	got := unescapePrintedDollars(`\$variable \\$literal \${value} \$9 \$(command)`)
	want := `$variable \$literal ${value} \$9 \$(command)`
	if got != want {
		t.Fatalf("printing layer = %q, want %q", got, want)
	}
}
