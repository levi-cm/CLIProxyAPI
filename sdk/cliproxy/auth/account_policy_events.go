package auth

import (
	"strconv"
	"strings"
)

// SetPolicyRefreshCallback installs an optional observation notification. Hosts
// should enqueue discovery here; this callback must not poll the provider inline.
// Existing credential lifecycle and result hooks remain independently installed.
func (m *Manager) SetPolicyRefreshCallback(callback func(string)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.policyRefreshCallback = callback
	m.mu.Unlock()
}

func (m *Manager) requestPolicyRefresh(id string) {
	if m == nil || id == "" {
		return
	}
	m.mu.RLock()
	callback := m.policyRefreshCallback
	m.mu.RUnlock()
	if callback != nil {
		callback(id)
	}
}

func policyIdentityMaterialChanged(before, after *Auth) bool {
	for _, key := range []string{"account_id", "accountId", "workspace_id", "workspaceId", "chatgpt_account_id"} {
		if authMetadataString(before, key) != authMetadataString(after, key) {
			return true
		}
	}
	return before.Provider != after.Provider
}

// Passive exhaustion triggers a coalesced read only. Discovery validates the
// actual duration/scope before routing or automation may act on this signal.
func policyQuotaExhausted(quota QuotaState) bool {
	for key, value := range quota.Signals {
		key = strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(key, "x-codex-") {
			continue
		}
		normalized := strings.ToLower(strings.TrimSpace(value))
		if strings.HasSuffix(key, "-allowed") && (normalized == "false" || normalized == "0") {
			return true
		}
		if strings.HasSuffix(key, "-limit-reached") && (normalized == "true" || normalized == "1") {
			return true
		}
		if strings.HasSuffix(key, "-used-percent") {
			if used, err := strconv.ParseFloat(normalized, 64); err == nil && used >= 100 {
				return true
			}
		}
	}
	return false
}

func (m *Manager) observePolicyResult(result Result, quotaTransition bool) {
	if result.SkipQuotaObservation || (!quotaTransition && statusCodeFromResult(result.Error) != 429) {
		return
	}
	if lease, ok := result.Options.Metadata[policyRequestLeaseMetadataKey].(*policyRequestLease); ok {
		lease.mu.Lock()
		if lease.streaming {
			lease.refreshPending = true
			lease.mu.Unlock()
			return
		}
		lease.mu.Unlock()
	}
	m.requestPolicyRefresh(result.AuthID)
}
