package accountpolicy

import (
	"math"
	"slices"
	"time"
)

// Error exposes a stable code without including upstream bodies or credentials.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string               { return e.Code + ": " + e.Message }
func policyError(code, message string) error { return &Error{Code: code, Message: message} }

func DefaultSettings() Settings {
	return Settings{Mode: "earliest_deadline", Automation: "off", Fallback: "round-robin", Affinity: "deadline_at_boundary", TimeZone: "Europe/Berlin", ExpiryGuardSeconds: 600, FreshnessSeconds: 120, SavedCreditReserve: 1, CreditTypes: []string{"codex_rate_limits"}, Accounts: map[string]AccountControl{}}
}

func ValidateSettings(s Settings) error {
	bad := func() error { return policyError("invalid_settings", "invalid account policy settings") }
	if s.Mode != "earliest_deadline" || !slices.Contains([]string{"off", "notify", "auto_expiring", "auto_all_saved"}, s.Automation) || !slices.Contains([]string{"strict", "deadline_at_boundary"}, s.Affinity) || !slices.Contains([]string{"round-robin", "fill-first", "weighted-round-robin"}, s.Fallback) {
		return bad()
	}
	if s.ExpiryGuardSeconds < 0 || s.ExpiryGuardSeconds > 86400 || s.FreshnessSeconds < 1 || s.FreshnessSeconds > 120 || s.SavedCreditReserve < 0 {
		return bad()
	}
	if _, err := time.LoadLocation(s.TimeZone); err != nil {
		return bad()
	}
	for _, kind := range s.CreditTypes {
		if kind != "codex_rate_limits" {
			return bad()
		}
	}
	for id, c := range s.Accounts {
		if id == "" || math.IsNaN(c.ReservePercent) || math.IsInf(c.ReservePercent, 0) || c.ReservePercent < 0 || c.ReservePercent > 100 {
			return bad()
		}
	}
	return nil
}

func fresh(at, now time.Time, seconds int) bool {
	return !at.IsZero() && !at.After(now) && now.Sub(at) <= time.Duration(seconds)*time.Second
}
func applicable(b Bucket, model string) bool {
	return (b.Scope == "ordinary" || b.Scope == "" || (b.Model != "" && b.Scope != "code_review")) && (b.Model == "" || b.Model == model)
}
func usableCredit(c Credit, s Settings, now time.Time) bool {
	return c.ID != "" && c.DetailsKnown && c.Status == "available" && c.Type == "codex_rate_limits" && slices.Contains(s.CreditTypes, c.Type) && slices.Contains(c.Scopes, "ordinary") && (c.ExpiresAt == nil || c.ExpiresAt.After(now))
}

// Evaluate is read-only and never polls or redeems. Known blocks survive stale evidence.
func Evaluate(snapshot Snapshot, model string, s Settings, now time.Time) Evaluation {
	result := Evaluation{Eligible: true}
	blocked := func(reason string) Evaluation { return Evaluation{Known: true, Reason: reason} }
	if !s.Enabled {
		result.Reason = "policy_disabled"
		return result
	}
	control := s.Accounts[snapshot.Identity.CredentialID]
	if control.Hold {
		return blocked("account_hold")
	}
	if s.ForceAccount != "" && s.ForceAccount != snapshot.Identity.CredentialID {
		return blocked("different_forced_account")
	}
	if snapshot.Status != "" && snapshot.Status != "healthy" && snapshot.Status != "quota" && snapshot.Status != "cooldown" {
		return blocked("authentication_or_account_unavailable")
	}
	if !snapshot.Eligible && !snapshot.ObservedAt.IsZero() {
		return blocked("account_ineligible")
	}
	known := fresh(snapshot.ObservedAt, now, s.FreshnessSeconds) && snapshot.LastError == ""
	for _, b := range snapshot.Buckets {
		if !applicable(b, model) {
			continue
		}
		if b.Allowed != nil && !*b.Allowed || b.UsedPercent >= 100 {
			return blocked("quota_block")
		}
		if control.ReservePercent > 0 && b.UsedPercent >= 100-control.ReservePercent {
			return blocked("account_reserve")
		}
		if math.IsNaN(b.UsedPercent) || b.UsedPercent < 0 || b.UsedPercent > 100 || !fresh(b.ObservedAt, now, s.FreshnessSeconds) {
			known = false
			continue
		}
		if b.DurationSeconds == 604800 && b.ResetAt.After(now) && (result.Deadline.IsZero() || b.ResetAt.Before(result.Deadline)) {
			result.Deadline = b.ResetAt
			result.Reason = "normal_weekly_reset"
		}
	}
	if !known || result.Deadline.IsZero() {
		result.Deadline = time.Time{}
		result.Reason = "missing_or_stale_evidence"
		return result
	}
	if s.Automation != "off" && fresh(snapshot.InventoryObservedAt, now, s.FreshnessSeconds) && !snapshot.WritesDisabled && !s.ReadOnly {
		for _, credit := range snapshot.Credits {
			if !usableCredit(credit, s, now) || credit.ExpiresAt == nil {
				continue
			}
			deadline := credit.ExpiresAt.Add(-time.Duration(s.ExpiryGuardSeconds) * time.Second)
			if deadline.Before(result.Deadline) {
				result.Deadline = deadline
				result.Reason = "expiring_reset_credit"
			}
		}
	}
	result.Known = true
	return result
}
