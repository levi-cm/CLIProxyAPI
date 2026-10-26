//go:build !linux

package auth

import "fmt"

func capacityAvailableMemory() (int64, error) {
	return 0, fmt.Errorf("capacity memory admission measurement is only supported on Linux")
}
