package helps

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter accepts RFC delay-seconds or an HTTP date without overflowing
// a duration. A valid elapsed deadline is represented by a non-nil zero delay.
func ParseRetryAfter(value string, now time.Time) *time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	digits := true
	for _, r := range value {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return nil
		}
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	delay := deadline.Sub(now)
	if delay < 0 {
		delay = 0
	}
	if delay == time.Duration(math.MaxInt64) {
		return nil
	}
	return &delay
}
