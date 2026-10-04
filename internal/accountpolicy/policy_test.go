package accountpolicy

import (
	"testing"
	"time"
)

func testTime() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }
func testSnapshot(id string, days int) Snapshot {
	now := testTime()
	allowed := true
	return Snapshot{Identity: Identity{CredentialID: id, AccountID: id, WorkspaceID: id, Provider: "codex", Generation: 1}, Eligible: true, Status: "healthy", ObservedAt: now, InventoryObservedAt: now, InventoryComplete: true, Buckets: []Bucket{{Scope: "ordinary", DurationSeconds: 18000, UsedPercent: 20, Allowed: &allowed, ResetAt: now.Add(time.Hour), ObservedAt: now}, {Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 40, Allowed: &allowed, ResetAt: now.Add(time.Duration(days) * 24 * time.Hour), ObservedAt: now}}}
}
func testCredit(id string, expiry *time.Time) Credit {
	return Credit{ID: id, Type: "codex_rate_limits", Status: "available", DetailsKnown: true, ExpiresAt: expiry, Scopes: []string{"ordinary"}}
}

// Removing expiry priority, reserve enforcement, or freshness handling must fail these cases.
func TestEvaluateDeadlineAndEligibility(t *testing.T) {
	now := testTime()
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	a, b := testSnapshot("a", 4), testSnapshot("b", 7)
	expiry := now.Add(24 * time.Hour)
	if !Evaluate(a, "model", settings, now).Deadline.Before(Evaluate(b, "model", settings, now).Deadline) {
		t.Fatal("A must precede B without credits")
	}
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	e := Evaluate(b, "model", settings, now)
	if !e.Known || !e.Eligible || !e.Deadline.Equal(now.Add(23*time.Hour+50*time.Minute)) {
		t.Fatalf("credit deadline: %+v", e)
	}
	if !e.Deadline.Before(Evaluate(a, "model", settings, now).Deadline) {
		t.Fatal("B expiry must precede A weekly reset")
	}
	b.Buckets[0].UsedPercent = 100
	if Evaluate(b, "model", settings, now).Eligible {
		t.Fatal("short exhaustion eligible")
	}
	b.Buckets[0].UsedPercent = 20
	settings.Accounts["b"] = AccountControl{ReservePercent: 60}
	if Evaluate(b, "model", settings, now).Eligible {
		t.Fatal("reserve boundary eligible")
	}
	settings.Accounts = map[string]AccountControl{}
	b.Buckets = append(b.Buckets, Bucket{Scope: "ordinary", Model: "blocked", UsedPercent: 100, ObservedAt: now})
	if Evaluate(b, "blocked", settings, now).Eligible {
		t.Fatal("model block eligible")
	}
	if !Evaluate(b, "other", settings, now).Eligible {
		t.Fatal("other model blocked")
	}
	b.ObservedAt = now.Add(-121 * time.Second)
	e = Evaluate(b, "other", settings, now)
	if e.Known || !e.Deadline.IsZero() {
		t.Fatalf("stale invented deadline: %+v", e)
	}
	b.Buckets[0].UsedPercent = 100
	if Evaluate(b, "other", settings, now).Eligible {
		t.Fatal("stale known block ignored")
	}
}

func TestEvaluateDoesNotInventInventoryOrSpendUnauthorizedCredits(t *testing.T) {
	now := testTime()
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	s := testSnapshot("b", 7)
	for _, credit := range []Credit{{ID: "count-only", Type: "codex_rate_limits", Status: "available"}, testCredit("saved", nil)} {
		s.AvailableCredits = 3
		s.Credits = []Credit{credit}
		e := Evaluate(s, "model", settings, now)
		if !e.Deadline.Equal(now.Add(7 * 24 * time.Hour)) {
			t.Fatalf("invented credit expiry: %+v", e)
		}
	}
	expiry := now.Add(time.Hour)
	c := testCredit("purchased", &expiry)
	c.Type = "purchased"
	s.Credits = []Credit{c}
	if !Evaluate(s, "model", settings, now).Deadline.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Fatal("purchased reset authorized")
	}
	c.Type = "codex_rate_limits"
	s.Credits = []Credit{c}
	settings.Automation = "off"
	if !Evaluate(s, "model", settings, now).Deadline.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Fatal("off automation prioritizes unauthorized redemption")
	}
}

func TestSettingsValidateSafety(t *testing.T) {
	s := DefaultSettings()
	if s.Enabled || s.Automation != "off" {
		t.Fatal("writes enabled by default")
	}
	if err := ValidateSettings(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Settings){func(s *Settings) { s.FreshnessSeconds = 121 }, func(s *Settings) { s.Automation = "purchase" }, func(s *Settings) { s.CreditTypes = []string{"purchased"} }, func(s *Settings) { s.Accounts["a"] = AccountControl{ReservePercent: 101} }, func(s *Settings) { s.TimeZone = "garbage" }} {
		s = DefaultSettings()
		mutate(&s)
		if ValidateSettings(s) == nil {
			t.Fatal("unsafe settings accepted")
		}
	}
}
