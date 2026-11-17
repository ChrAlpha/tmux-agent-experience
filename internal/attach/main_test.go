package attach

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// A session that reconciliation ends starts `tmag notify --ended` from
	// os.Executable, which in these tests is this binary: run as that, it
	// settles nothing, rather than running every test again.
	if len(os.Args) > 1 && os.Args[1] == "notify" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
