//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package accountpolicy

func lockJournal(string) (func(), error) {
	return nil, policyError("read_only", "automatic redemption requires supported process locking on this platform")
}

func lockNamed(string, string) (func(), error) {
	return nil, policyError("read_only", "durable policy writes require supported process locking on this platform")
}
