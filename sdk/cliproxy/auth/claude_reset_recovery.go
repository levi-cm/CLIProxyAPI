package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// ClaudeResetRecovery describes a spent Claude reset grant for local recovery.
type ClaudeResetRecovery struct {
	CredentialID string
	// AccountUUID and OrganizationUUID must still match the credential.
	AccountUUID      string
	OrganizationUUID string
	// ClearsWeekly restores every model; ClearsFable restores Fable models and
	// reopens the Fable window.
	ClearsWeekly bool
	ClearsFable  bool
	// WeeklyResetAt and FableResetAt are the cleared windows' renewal times at
	// claim time. Only quota cooldowns ending at one of them were caused by a
	// cleared window; other cooldowns (a 5-hour window, unknown causes) stay.
	WeeklyResetAt time.Time
	FableResetAt  time.Time
	// WeeklyOpen, FableOpen and SessionFull come from a verified inventory read
	// taken after the reset (at ObservedAt). A cooldown is cleared only when
	// that read shows the window behind it open. Cooldowns recorded after the
	// read are newer failures and stay.
	WeeklyOpen     bool
	FableOpen      bool
	SessionFull    bool
	SessionResetAt time.Time
	ObservedAt     time.Time
	// ConfirmedAt is when the reset was confirmed; window observations up to it
	// are ignored.
	ConfirmedAt time.Time
	// Pass, when set, limits this call to windows newly confirmed in this
	// pass; an earlier pass already recovered the others. Nil recovers all.
	Pass *ClaudeResetPass
	// Fenced means the caller recorded the execution watermark before its
	// read (fenceClaudeRecovery). A later watermark would also drop evidence
	// of requests dispatched after the reset, so none is recorded here.
	Fenced bool
}

// ClaudeResetPass names the windows one recovery pass newly confirmed.
type ClaudeResetPass struct {
	Weekly bool
	Fable  bool
}

func (r ClaudeResetRecovery) passWeekly() bool { return r.Pass == nil || r.Pass.Weekly }
func (r ClaudeResetRecovery) passFable() bool  { return r.Pass == nil || r.Pass.Fable }

// claudeCooldownCauseSlack absorbs the fuzz added to a window's reset deadline
// when a refusal sets its cooldown.
const claudeCooldownCauseSlack = 30 * time.Minute

// causedBy reports whether a quota cooldown ending at until was set by the
// window renewing at reset.
func causedBy(until, reset time.Time) bool {
	return !until.IsZero() && !reset.IsZero() && !until.Before(reset.Add(-2*time.Minute)) && !until.After(reset.Add(claudeCooldownCauseSlack))
}

// cleared reports whether recovery r restored the quota a cooldown on model
// ending at until was waiting for. A deadline that also matches a still-full
// 5-hour window is ambiguous and stays.
func (r ClaudeResetRecovery) cleared(model string, until time.Time) bool {
	if r.SessionFull && causedBy(until, r.SessionResetAt) {
		return false
	}
	if claudeFableModel(model) {
		// Fable needs both windows open; its cooldown may come from either.
		return r.FableOpen && r.WeeklyOpen && ((r.ClearsFable && r.passFable() && causedBy(until, r.FableResetAt)) || (r.ClearsWeekly && r.passWeekly() && causedBy(until, r.WeeklyResetAt)))
	}
	return r.ClearsWeekly && r.passWeekly() && r.WeeklyOpen && causedBy(until, r.WeeklyResetAt)
}

func quotaCooldownUntil(q QuotaState, retry time.Time) time.Time {
	if !q.NextRecoverAt.IsZero() {
		return q.NextRecoverAt
	}
	return retry
}

var errClaudeRecoveryIdentity = errors.New("claude reset recovery: credential identity changed")

// RecoverClaudeReset reopens routing for the allowance a spent reset restored,
// without broad ResetQuota semantics. Like RecoverPolicyQuota it clears only
// quota cooldowns that a cleared window caused and that were recorded before
// the reset was confirmed, persists the credential and the cooldown store
// before publishing, and records a recovery watermark so results of executions
// dispatched before the recovery cannot re-apply cooldowns or quota headers.
// It reports an error unless all of that succeeded.
func (m *Manager) RecoverClaudeReset(ctx context.Context, r ClaudeResetRecovery) error {
	if m == nil {
		return errors.New("claude reset recovery: manager unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !r.ClearsWeekly && !r.ClearsFable {
		return nil
	}
	id := r.CredentialID
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, id)
	if errMutation != nil {
		return errMutation
	}
	defer releaseMutation()
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.Lock()
	auth := m.auths[id]
	if auth == nil || !isClaudeAuth(auth) {
		m.mu.Unlock()
		return fmt.Errorf("claude reset recovery: credential %s unavailable", id)
	}
	if account, org := claudeResetIdentity(auth); account == "" || account != r.AccountUUID || org != r.OrganizationUUID {
		m.mu.Unlock()
		return errClaudeRecoveryIdentity
	}
	epoch := auth.RegistrationEpoch
	if auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		m.mu.Unlock()
		return nil
	}
	recoverySeq := m.currentExecutionSeq()
	global := r.ClearsWeekly && r.passWeekly()
	covered := map[string]bool{}
	if !global && r.passFable() {
		for model := range auth.ModelStates {
			if claudeFableModel(model) {
				covered[canonicalModelKey(model)] = true
			}
		}
		for _, model := range registry.GetGlobalRegistry().GetModelsForClient(id) {
			if model != nil && claudeFableModel(model.ID) {
				covered[canonicalModelKey(model.ID)] = true
			}
		}
	}
	publishedBefore := auth.Clone()
	auth = auth.Clone()
	now := time.Now()
	changed := false
	for model, state := range auth.ModelStates {
		if state == nil || state.Status == StatusDisabled || state.UpdatedAt.After(r.ObservedAt) || !policyQuotaFailure(state.Quota, state.LastError) {
			continue
		}
		if !r.cleared(model, quotaCooldownUntil(state.Quota, state.NextRetryAfter)) {
			continue
		}
		resetModelState(state, now)
		changed = true
	}
	// The credential-wide cooldown has no failure timestamp of its own (passive
	// reads advance UpdatedAt), so the post-reset read alone decides it.
	authUntil := quotaCooldownUntil(auth.Quota, auth.NextRetryAfter)
	if global && r.WeeklyOpen && policyQuotaFailure(auth.Quota, auth.LastError) && causedBy(authUntil, r.WeeklyResetAt) && !(r.SessionFull && causedBy(authUntil, r.SessionResetAt)) {
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
		if !r.Fenced {
			m.recordPolicyQuotaRecoveryLocked(id, covered, global, recoverySeq)
		}
		m.mu.Unlock()
		releaseMutation()
		m.forgetClaudeResetWindow(id, epoch, r)
		return nil
	}
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
	if errPersist := m.persistLocked(context.WithoutCancel(ctx), auth); errPersist != nil {
		m.mu.Unlock()
		return fmt.Errorf("claude reset recovery: persist credential: %w", errPersist)
	}
	mergeAuthSaveDelta(auth, publishedBefore, m.auths[id], false)
	snapshot := auth.Clone()
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
			return fmt.Errorf("claude reset recovery: persist cooldown: %w", errSave)
		}
	}
	m.auths[id] = auth
	if !r.Fenced {
		m.recordPolicyQuotaRecoveryLocked(id, covered, global, recoverySeq)
	}
	m.mu.Unlock()
	releaseMutation()
	supported, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(id)
	projections := make([]registry.ClientModelProjection, 0, len(supported))
	for _, model := range supported {
		if model != nil && strings.TrimSpace(model.ID) != "" {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	registry.GetGlobalRegistry().ApplyClientModelProjections(id, regEpoch, snapshot.Generation, projections)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	m.forgetClaudeResetWindow(id, epoch, r)
	return nil
}

// fenceClaudeRecovery records the recovery watermark for a spent reset before
// its recovery reads inventory, so results of executions dispatched before now
// cannot apply cooldowns or quota headers for the restored models while the
// read runs. It reports false when the credential holds another identity.
func (m *Manager) fenceClaudeRecovery(id, accountUUID, orgUUID string, global bool) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[id]
	if auth == nil || !isClaudeAuth(auth) {
		return false
	}
	if account, org := claudeResetIdentity(auth); account == "" || account != accountUUID || org != orgUUID {
		return false
	}
	covered := map[string]bool{}
	if !global {
		for model := range auth.ModelStates {
			if claudeFableModel(model) {
				covered[canonicalModelKey(model)] = true
			}
		}
		for _, model := range registry.GetGlobalRegistry().GetModelsForClient(id) {
			if model != nil && claudeFableModel(model.ID) {
				covered[canonicalModelKey(model.ID)] = true
			}
		}
	}
	m.recordPolicyQuotaRecoveryLocked(id, covered, global, m.currentExecutionSeq())
	return true
}

// forgetClaudeResetWindow reopens the Fable window a reset cleared, for the
// recovered registration only.
func (m *Manager) forgetClaudeResetWindow(id string, epoch uint64, r ClaudeResetRecovery) {
	if !r.ClearsFable || !r.FableOpen || !r.passFable() {
		return
	}
	if forgetter, ok := m.Selector().(claudeLimitForgetter); ok && forgetter != nil {
		forgetter.ForgetClaudeLimits(id, epoch, []string{claudeFableWindow}, r.ConfirmedAt)
	}
}
