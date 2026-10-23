package accountpolicy

import "slices"

const minimumAutomaticExpiryGuard = 600

func automaticRedemption(mode string) bool {
	return mode == "auto_expiring" || mode == "auto_all_saved"
}

// EnforceAutomationRequirement normalizes startup settings only. Once an
// automatic mode is selected, its requirement is permanent in the policy journal.
// Explicit read-only replica configuration retains its ownership restriction.
// Runtime mutations must be rejected instead of silently repaired.
func EnforceAutomationRequirement(settings Settings) Settings {
	if !settings.AutomationRequired && !automaticRedemption(settings.Automation) {
		return settings
	}
	settings.AutomationRequired = true
	settings.Enabled = true
	if !automaticRedemption(settings.Automation) {
		settings.Automation = "auto_expiring"
	}
	if settings.ExpiryGuardSeconds < minimumAutomaticExpiryGuard {
		settings.ExpiryGuardSeconds = minimumAutomaticExpiryGuard
	}
	if !slices.Contains(settings.CreditTypes, "codex_rate_limits") {
		settings.CreditTypes = append(slices.Clone(settings.CreditTypes), "codex_rate_limits")
	}
	return settings
}

func (s *Service) checkAutomationRequirementLocked(next Settings) (Settings, error) {
	required := s.state.Settings.AutomationRequired || next.AutomationRequired || automaticRedemption(next.Automation)
	if !required {
		return next, nil
	}
	removingLock := s.state.Settings.AutomationRequired && !next.AutomationRequired
	disablingWrites := next.ReadOnly && !s.opts.Settings.ReadOnly && !s.replica
	if removingLock || !next.Enabled || !automaticRedemption(next.Automation) || disablingWrites || next.ExpiryGuardSeconds < minimumAutomaticExpiryGuard || !slices.Contains(next.CreditTypes, "codex_rate_limits") {
		return next, policyError("automation_required", "automatic reset redemption is permanently required and cannot be disabled")
	}
	if s.stateDir == "" {
		return next, policyError("persistence", "permanent automatic redemption requires durable policy state")
	}
	next.AutomationRequired = true
	return next, nil
}
