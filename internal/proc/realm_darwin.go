package proc

import "syscall"

// realmSource is the boot session: macOS has one process table per boot.
func realmSource() string {
	id, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return id
}
