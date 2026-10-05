//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package accountpolicybindings

import "os"

const noFollow = 0

func lock(*os.Root) (func(), error) { return nil, ErrUnavailable }
