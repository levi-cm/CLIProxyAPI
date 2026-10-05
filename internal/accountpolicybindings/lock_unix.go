//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package accountpolicybindings

import (
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func lock(root *os.Root) (func(), error) {
	f, err := root.OpenFile("conversation-bindings.lock", os.O_RDWR|os.O_CREATE|noFollow, 0600)
	if err != nil {
		return nil, ErrUnavailable
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return nil, ErrUnavailable
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, ErrUnavailable
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
