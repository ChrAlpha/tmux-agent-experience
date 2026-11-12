package gateway

import (
	"os"
	"strings"
	"testing"
)

// The composer follows the send capability; tmag's gate decides the state.
func TestComposerFollowsSendCapability(t *testing.T) {
	b, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if strings.Contains(page, `caps.includes("send") && x.s.state === "idle"`) {
		t.Fatal("the composer is limited to idle sessions")
	}
	if !strings.Contains(page, `const canSend = caps.includes("send");`) {
		t.Fatal("the composer does not follow the send capability")
	}
}
