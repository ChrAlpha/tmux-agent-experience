package dirscan

import "syscall"

func readPage(fd int, buf []byte) (int, error) { return syscall.ReadDirent(fd, buf) }
