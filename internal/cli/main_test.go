package cli

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Children of the test binary (TestHookCLIInheritedInput) are timed as
	// hooks are, without the race runtime's artificial process-exit sleep.
	if err := os.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
