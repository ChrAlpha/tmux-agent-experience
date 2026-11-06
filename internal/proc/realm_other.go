//go:build !darwin

package proc

import (
	"os"
	"strings"
)

// realmSource is the boot and the PID namespace: Linux shows each namespace
// its own process table, and every container on a host shares its boot id.
func realmSource() string {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(boot)) + " " + ns
}
