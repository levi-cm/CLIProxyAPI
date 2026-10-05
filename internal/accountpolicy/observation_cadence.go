package accountpolicy

import "time"

// Healthy observation cadence stays inside the evidence freshness window. Error
// retry delays are owned by discovery's separate backoff path and are untouched.
func observationDelay(interval time.Duration, freshnessSeconds int, credentialID string) time.Duration {
	interval = min(interval, time.Duration(freshnessSeconds)*time.Second/2)
	var hash uint32
	for _, r := range credentialID {
		hash = hash*31 + uint32(r)
	}
	jitter := min(time.Duration(hash%6)*time.Second, interval/10)
	return interval + jitter
}
