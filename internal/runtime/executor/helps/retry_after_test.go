package helps

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"120", 2 * time.Minute, true}, {"0", 0, true}, {now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute, true}, {now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-1", 0, false}, {"NaN", 0, false}, {"Inf", 0, false}, {"1.2", 0, false}, {"", 0, false}, {"9223372036854775807", 0, false}, {"99999999999999999999999999", 0, false},
	} {
		got := ParseRetryAfter(tc.value, now)
		if (got != nil) != tc.valid || got != nil && *got != tc.want {
			t.Errorf("%q: %v want %v valid %v", tc.value, got, tc.want, tc.valid)
		}
	}
}
