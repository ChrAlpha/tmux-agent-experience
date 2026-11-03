package hook

import "testing"

func TestAlert(t *testing.T) {
	for _, c := range []struct{ prev, next, want string }{
		{"working", "blocked", "blocked"},
		{"", "blocked", "blocked"},
		{"blocked", "blocked", ""},
		{"working", "idle", "finished"},
		{"blocked", "working", ""},
		{"idle", "idle", ""},
		{"", "idle", ""},
	} {
		if got := Alert(c.prev, c.next); got != c.want {
			t.Errorf("Alert(%q, %q) = %q, want %q", c.prev, c.next, got, c.want)
		}
	}
}
