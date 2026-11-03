package tmux

import "testing"

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"3.2": true, "3.3a": true, "3.8": true, "4.0": true, "3.1c": false, "2.9": false,
		"next-3.9": true, "master": true, "openbsd-7.6": true,
	} {
		if got := VersionAtLeast(v, 3, 2); got != want {
			t.Errorf("VersionAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSocketFromEnv(t *testing.T) {
	if got := SocketFromEnv("/tmp/tmux-1000/default,117113,5"); got != "/tmp/tmux-1000/default" {
		t.Fatal(got)
	}
}
