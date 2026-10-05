// Package accountpolicyclient exposes sanitized, read-only, session-scoped quota
// reports. It does not select accounts, poll providers, or redeem resets.
package accountpolicyclient

import (
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

type Weekly struct {
	Status           string     `json:"status"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent"`
	WindowStartsAt   *time.Time `json:"window_starts_at"`
	RefreshAt        *time.Time `json:"refresh_at"`
	ObservedAt       *time.Time `json:"observed_at"`
	DurationSeconds  int64      `json:"duration_seconds"`
}

type Account struct {
	CredentialID   string `json:"credential_id"`
	Provider       string `json:"provider"`
	Label          string `json:"label"`
	ActiveRequests int    `json:"active_requests"`
	Weekly         Weekly `json:"weekly"`
}

type Report struct {
	SchemaVersion         int        `json:"schema_version"`
	SessionID             string     `json:"session_id"`
	SampledAt             time.Time  `json:"sampled_at"`
	TimeZone              string     `json:"time_zone"`
	State                 string     `json:"state"`
	ActiveAccounts        []Account  `json:"active_accounts"`
	LastSuccessfulAccount *Account   `json:"last_successful_account"`
	LastCompletedAt       *time.Time `json:"last_completed_at"`
}

// WeeklyUsage never converts a missing observation into zero usage or infers a
// reset from subscription renewal. Multiple ordinary weekly buckets are ambiguous.
func WeeklyUsage(snapshot accountpolicy.Snapshot, freshnessSeconds int, now time.Time) Weekly {
	out := Weekly{Status: "unavailable", DurationSeconds: 604800}
	fresh := func(at time.Time) bool {
		return freshnessSeconds > 0 && !at.IsZero() && !at.After(now) && now.Sub(at) <= time.Duration(freshnessSeconds)*time.Second
	}
	if snapshot.LastError != "" || !fresh(snapshot.ObservedAt) {
		out.Status = "stale_or_unavailable"
		return out
	}
	var weekly *accountpolicy.Bucket
	for i := range snapshot.Buckets {
		b := &snapshot.Buckets[i]
		if b.Scope == "ordinary" && b.Model == "" && b.DurationSeconds == 604800 {
			if weekly != nil {
				return out
			}
			weekly = b
		}
	}
	if weekly == nil {
		return out
	}
	// Allow one minute of provider/client clock skew, not a future weekly cycle.
	if !fresh(weekly.ObservedAt) || !weekly.ResetAt.After(now) || weekly.ResetAt.After(weekly.ObservedAt.Add(7*24*time.Hour+time.Minute)) {
		out.Status = "stale_or_unavailable"
		return out
	}
	if math.IsNaN(weekly.UsedPercent) || math.IsInf(weekly.UsedPercent, 0) || weekly.UsedPercent < 0 || weekly.UsedPercent > 100 {
		return out
	}
	used, remaining := weekly.UsedPercent, 100-weekly.UsedPercent
	reset, observed := weekly.ResetAt.UTC(), weekly.ObservedAt.UTC()
	start := reset.Add(-7 * 24 * time.Hour)
	out.Status, out.UsedPercent, out.RemainingPercent = "fresh", &used, &remaining
	out.RefreshAt, out.WindowStartsAt, out.ObservedAt = &reset, &start, &observed
	return out
}
