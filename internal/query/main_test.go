package query

import (
	"os"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/socketbind"
)

func TestMain(m *testing.M) {
	if handled, err := socketbind.RunChild(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Child setup/connection budgets measure work, including instrumented builds,
	// without the race runtime's artificial process-exit sleep.
	if err := os.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
