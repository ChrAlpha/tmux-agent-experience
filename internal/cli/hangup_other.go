//go:build !linux && !darwin

package cli

import "os"

// hangup is not known on this platform; tmag supports Linux and macOS.
func hangup(*os.File, <-chan struct{}) <-chan struct{} { return nil }
