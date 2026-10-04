//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package accountpolicy

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockNamed(dir, name string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, policyError("persistence", "cannot open redemption ownership lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, policyError("conflict", "another proxy owns account redemption")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func lockJournal(dir string) (func(), error) { return lockNamed(dir, "redemption.lock") }
