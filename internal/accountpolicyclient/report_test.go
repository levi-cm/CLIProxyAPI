package accountpolicyclient

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

// Model-specific or stale quota must never masquerade as ordinary weekly usage.
func TestWeeklyUsageUsesOnlyFreshOrdinarySevenDayWindow(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 10, 9, 21, 16, 42, 0, time.UTC)
	bucket := accountpolicy.Bucket{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 8, ResetAt: reset, ObservedAt: now}
	snapshot := accountpolicy.Snapshot{ObservedAt: now, Buckets: []accountpolicy.Bucket{bucket}}
	got := WeeklyUsage(snapshot, 120, now)
	if got.Status != "fresh" || got.UsedPercent == nil || *got.UsedPercent != 8 || got.RemainingPercent == nil || *got.RemainingPercent != 92 || got.RefreshAt == nil || !got.RefreshAt.Equal(reset) || got.WindowStartsAt == nil || got.WindowStartsAt.Format(time.RFC3339) != "2026-10-02T21:16:42Z" {
		t.Fatalf("wrong weekly percentage or window: %+v", got)
	}
	for _, test := range []struct {
		name   string
		change func(*accountpolicy.Snapshot)
	}{
		{"stale bucket", func(s *accountpolicy.Snapshot) { s.Buckets[0].ObservedAt = now.Add(-121 * time.Second) }},
		{"stale snapshot", func(s *accountpolicy.Snapshot) { s.ObservedAt = now.Add(-121 * time.Second) }},
		{"future evidence", func(s *accountpolicy.Snapshot) { s.Buckets[0].ObservedAt = now.Add(time.Second) }},
		{"refreshed cycle", func(s *accountpolicy.Snapshot) { s.Buckets[0].ResetAt = now }},
		{"impossible future window", func(s *accountpolicy.Snapshot) { s.Buckets[0].ResetAt = now.Add(14 * 24 * time.Hour) }},
		{"model bucket", func(s *accountpolicy.Snapshot) { s.Buckets[0].Model = "special" }},
		{"review scope", func(s *accountpolicy.Snapshot) { s.Buckets[0].Scope = "code_review" }},
		{"short window", func(s *accountpolicy.Snapshot) { s.Buckets[0].DurationSeconds = 18000 }},
		{"provider failed", func(s *accountpolicy.Snapshot) { s.LastError = "private provider error" }},
		{"invalid percent", func(s *accountpolicy.Snapshot) { s.Buckets[0].UsedPercent = 101 }},
		{"nonfinite percent", func(s *accountpolicy.Snapshot) { s.Buckets[0].UsedPercent = math.NaN() }},
		{"missing evidence", func(s *accountpolicy.Snapshot) { s.Buckets = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := snapshot
			s.Buckets = append([]accountpolicy.Bucket(nil), snapshot.Buckets...)
			test.change(&s)
			value := WeeklyUsage(s, 120, now)
			if value.Status == "fresh" || value.UsedPercent != nil || value.RemainingPercent != nil || value.RefreshAt != nil {
				t.Fatalf("unreliable evidence reported as quota: %+v", value)
			}
		})
	}
}
