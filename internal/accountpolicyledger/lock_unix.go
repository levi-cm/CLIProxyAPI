//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package accountpolicyledger

import (
	"os"
	"syscall"
)

func lockArchive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
