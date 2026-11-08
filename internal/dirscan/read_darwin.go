package dirscan

import (
	"runtime"
	"syscall"
	"unsafe"
)

// Darwin's exported syscall.Getdirentries simulates a cookie by replaying a
// count of prior entries, so it cannot bound repeated incremental scans. XNU's
// actual getdirentries64 advances the descriptor's native directory position.
// The exported syscall constant exists on both supported Darwin architectures.
// ABI: fd, buffer, byte count, off_t *position (position is the page's START).
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/syscalls.master
func readPage(fd int, buf []byte) (int, error) {
	var start int64
	n, _, errno := syscall.Syscall6(syscall.SYS_GETDIRENTRIES64, uintptr(fd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&start)), 0, 0)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(&start)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}
