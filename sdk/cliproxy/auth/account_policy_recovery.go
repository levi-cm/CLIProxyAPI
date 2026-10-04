package auth

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func recoveryBucketUsable(b accountpolicy.Bucket) bool {
	return b.Allowed != nil && *b.Allowed && !math.IsNaN(b.UsedPercent) && !math.IsInf(b.UsedPercent, 0) && b.UsedPercent >= 0 && b.UsedPercent < 100
}

// policyRecoveredModels identifies only explicitly recovered ordinary scopes.
// An unspecified or still-blocked window cannot establish account-wide recovery.
func policyRecoveredModels(before, after accountpolicy.Snapshot) (map[string]bool, bool) {
	models := make(map[string]bool)
	global := false
	for _, old := range before.Buckets {
		if old.Scope != "ordinary" && old.Scope != "" {
			continue
		}
		for _, fresh := range after.Buckets {
			if fresh.Scope != old.Scope || fresh.Model != old.Model || fresh.DurationSeconds != old.DurationSeconds {
				continue
			}
			if !recoveryBucketUsable(fresh) || !fresh.ObservedAt.After(before.ObservedAt) || fresh.ObservedAt.After(after.ObservedAt) {
				continue
			}
			// Fresh provider allowance is authoritative even when passive inference
			// quota signals preceded the previous discovery snapshot.
			usable := true
			for _, bucket := range after.Buckets {
				if (bucket.Scope == "ordinary" || bucket.Scope == "") && (bucket.Model == "" || bucket.Model == fresh.Model) && !recoveryBucketUsable(bucket) {
					usable = false
					break
				}
			}
			for _, prior := range before.Buckets {
				if (prior.Scope != "ordinary" && prior.Scope != "") || (prior.Model != "" && prior.Model != fresh.Model) {
					continue
				}
				matched := false
				for _, observed := range after.Buckets {
					if observed.Scope == prior.Scope && observed.Model == prior.Model && observed.DurationSeconds == prior.DurationSeconds && recoveryBucketUsable(observed) && observed.ObservedAt.After(before.ObservedAt) && !observed.ObservedAt.After(after.ObservedAt) {
						matched = true
						break
					}
				}
				if !matched {
					usable = false
					break
				}
			}
			if !usable {
				continue
			}
			if fresh.Model == "" {
				global = true
			} else {
				models[canonicalModelKey(fresh.Model)] = true
			}
		}
	}
	return models, global
}

func policyQuotaFailure(quota QuotaState, lastError *Error) bool {
	return quota.Exceeded && (quota.Reason == "quota" || quota.Reason == "credential_quota") && (lastError == nil || statusCodeFromResult(lastError) == 429)
}

// RecoverPolicyQuota reconciles provider recovery without broad ResetQuota
// semantics. Identity, generation and timestamps guard every local mutation.
func (m *Manager) RecoverPolicyQuota(ctx context.Context, before, after accountpolicy.Snapshot) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if before.Identity.CredentialID != after.Identity.CredentialID || before.Identity.AccountID != after.Identity.AccountID || before.Identity.WorkspaceID != after.Identity.WorkspaceID || before.Identity.Provider != after.Identity.Provider || before.Identity.Generation != after.Identity.Generation || before.Identity.CredentialID == "" || before.Identity.AccountID == "" || before.Identity.WorkspaceID == "" || !after.ObservedAt.After(before.ObservedAt) {
		return &accountpolicy.Error{Code: "stale_recovery", Message: "recovery identity or observation does not match"}
	}
	covered, global := policyRecoveredModels(before, after)
	if len(covered) == 0 && !global {
		return nil
	}
	id := before.Identity.CredentialID
	// Keep the published credential blocked until both persistence layers accept
	// recovery, serialized with every independent cooldown save.
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.Lock()
	auth := m.auths[id]
	if auth == nil {
		m.mu.Unlock()
		return &accountpolicy.Error{Code: "account_not_found", Message: "account is unavailable"}
	}
	if auth.Provider != before.Identity.Provider || auth.Generation != before.Identity.Generation || auth.UpdatedAt.After(before.ObservedAt) {
		m.mu.Unlock()
		return &accountpolicy.Error{Code: "stale_recovery", Message: "credential changed after recovery evidence"}
	}
	if auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) || statusCodeFromResult(auth.LastError) == 401 || isInvalidGrantResultError(auth.LastError) {
		m.mu.Unlock()
		return nil
	}
	// Upstream ownership is also checked against the current credential metadata,
	// when the host exposes it. Missing metadata never supplies a different owner.
	for key, want := range map[string]string{"account_id": before.Identity.AccountID, "workspace_id": before.Identity.WorkspaceID} {
		if current, ok := auth.Metadata[key].(string); ok && current != "" && current != want {
			m.mu.Unlock()
			return &accountpolicy.Error{Code: "identity_mismatch", Message: "current credential ownership changed"}
		}
	}
	auth = auth.Clone()
	changed := false
	now := after.ObservedAt
	for model, state := range auth.ModelStates {
		if state == nil || state.Status == StatusDisabled || state.UpdatedAt.After(before.ObservedAt) || !policyQuotaFailure(state.Quota, state.LastError) {
			continue
		}
		if !global && !covered[canonicalModelKey(model)] {
			continue
		}
		resetModelState(state, now)
		changed = true
	}
	if global && policyQuotaFailure(auth.Quota, auth.LastError) {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		applyCooldownFields(&auth.Quota, QuotaState{})
		if auth.LastError != nil && statusCodeFromResult(auth.LastError) == 429 {
			auth.LastError = nil
			auth.StatusMessage = ""
		}
		changed = true
	}
	if !changed {
		m.mu.Unlock()
		return nil
	}
	// Aggregate model availability only after a genuine quota mutation. Preserve
	// credential-scoped nonquota failures and their retry deadline verbatim.
	if auth.LastError == nil || statusCodeFromResult(auth.LastError) == 429 {
		updateAggregatedAvailability(auth, now)
		if !hasModelError(auth, now) {
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
	}
	auth.Generation++
	auth.UpdatedAt = now
	snapshot := auth.Clone()
	errPersist := m.persist(context.WithoutCancel(ctx), auth)
	if errPersist != nil {
		m.mu.Unlock()
		return fmt.Errorf("persist recovered credential: %w", errPersist)
	}
	if store := m.cooldownStore; store != nil {
		records := make([]CooldownStateRecord, 0)
		for candidateID, candidate := range m.auths {
			if candidateID == id {
				candidate = auth
			}
			records = append(records, m.cooldownStateRecordsForAuthLocked(candidate, time.Now())...)
		}
		sort.Slice(records, func(i, j int) bool {
			if records[i].Provider != records[j].Provider {
				return records[i].Provider < records[j].Provider
			}
			if records[i].AuthID != records[j].AuthID {
				return records[i].AuthID < records[j].AuthID
			}
			return records[i].Model < records[j].Model
		})
		if errSave := store.Save(context.WithoutCancel(ctx), records); errSave != nil {
			m.mu.Unlock()
			return fmt.Errorf("persist recovered cooldown: %w", errSave)
		}
	}
	m.auths[id] = auth
	m.mu.Unlock()
	supported, epoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(id)
	projections := make([]registry.ClientModelProjection, 0, len(supported))
	for _, model := range supported {
		if model != nil && strings.TrimSpace(model.ID) != "" {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	registry.GetGlobalRegistry().ApplyClientModelProjections(id, epoch, snapshot.Generation, projections)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return nil
}
