// Package sockettest gives tests directories for Unix sockets.
package sockettest

import (
	"os"
	"testing"
)

// Dir returns a new directory for tb's Unix sockets under /tmp, as tmag
// keeps its own, and removes it when tb ends. A socket's path holds 103 bytes
// on macOS, whose $TMPDIR takes 48 of them; tb.TempDir adds the test's name,
// which leaves too few.
func Dir(tb testing.TB) string {
	tb.Helper()
	dir, err := os.MkdirTemp("/tmp", "tmag-test-")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
