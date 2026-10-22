//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package accountpolicyledger

import (
	"errors"
	"os"
)

func lockArchive(_ *os.File) error {
	return errors.New("durable history writer locking unavailable on this platform")
}
