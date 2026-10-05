package auth

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// Client history is ephemeral, bounded, and never influences selection.
const policyClientHistoryLimit = 4096
const policyClientHistoryTTL = 24 * time.Hour
const policyClientSelectionMetadataKey = "account_policy_client_selection"

type policyClientKey struct{ caller, session string }
type policyClientSelection struct {
	authID   string
	identity string
	epoch    uint64
	at       time.Time
}

// PolicyClientAccount is an immutable allowlist from the exact validated Auth
// clone. Inference/API consumers must not re-resolve its ID to a newer identity.
type PolicyClientAccount struct {
	CredentialID   string
	Provider       string
	Label          string
	ActiveRequests int
	Identity       *accountpolicy.Identity
}

func policyClientAccount(a *Auth, count int) PolicyClientAccount {
	view := PolicyClientAccount{CredentialID: a.ID, Provider: a.Provider, Label: a.Label, ActiveRequests: count}
	if email := authMetadataString(a, "email"); email != "" {
		view.Label = email
	}
	if identity, err := PolicyAccountIdentity(a); err == nil {
		view.Identity = &identity
	}
	return view
}

func policyClientIdentity(a *Auth) string {
	parts := []string{a.Provider}
	if identity, err := PolicyAccountIdentity(a); err == nil {
		parts = append(parts, identity.AccountID, identity.WorkspaceID)
	}
	for _, key := range []string{"account_id", "accountId", "workspace_id", "workspaceId", "chatgpt_account_id", "email"} {
		parts = append(parts, authMetadataString(a, key))
	}
	return strings.Join(parts, "\x00")
}

func policyClientKeyFromOptions(opts cliproxyexecutor.Options) policyClientKey {
	caller, _ := opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey].(string)
	thread := session.NormalizeExplicitID(opts.Headers.Get("Session_id"))
	if thread == "" {
		thread = session.NormalizeExplicitID(opts.Headers.Get("Session-Id"))
	}
	if thread == "" || caller == "" {
		return policyClientKey{}
	}
	return policyClientKey{caller, "codex:" + thread}
}

// Snapshot only the selected attempt's non-secret ownership fields. Never look
// up a replacement credential later and relabel an old in-flight request.
func capturePolicyClientSelection(meta map[string]any, a *Auth) {
	selection := policyClientSelection{authID: a.ID, identity: policyClientIdentity(a), epoch: a.RegistrationEpoch}
	meta[policyClientSelectionMetadataKey] = selection
	if lease, ok := meta[policyRequestLeaseMetadataKey].(*policyRequestLease); ok {
		lease.manager.policyRuntimeMu.Lock()
		lease.clientSelection = selection
		lease.manager.policyRuntimeMu.Unlock()
	}
}

func (m *Manager) recordPolicyClientResult(result Result) {
	if m == nil || !result.Success || result.SkipQuotaObservation || result.Provider != "codex" {
		return
	}
	key := policyClientKeyFromOptions(result.Options)
	if key.caller == "" || key.session == "" {
		return
	}
	selection, ok := result.Options.Metadata[policyClientSelectionMetadataKey].(policyClientSelection)
	if !ok || selection.authID != result.AuthID {
		return
	}
	// Hold auth ownership stable through the history update. A late completion
	// from a replaced identity must not overwrite that replacement's success.
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.auths[selection.authID]
	if a == nil || a.RegistrationEpoch != selection.epoch || policyClientIdentity(a) != selection.identity {
		return
	}
	now := time.Now().UTC()
	m.policyRuntimeMu.Lock()
	defer m.policyRuntimeMu.Unlock()
	if m.policyClientHistory == nil {
		m.policyClientHistory = make(map[policyClientKey]policyClientSelection)
	}
	if _, exists := m.policyClientHistory[key]; !exists && len(m.policyClientHistory) >= policyClientHistoryLimit {
		var oldest policyClientKey
		var at time.Time
		for candidate, selection := range m.policyClientHistory {
			if at.IsZero() || selection.at.Before(at) {
				oldest, at = candidate, selection.at
			}
		}
		delete(m.policyClientHistory, oldest)
	}
	selection.at = now
	m.policyClientHistory[key] = selection
}

// PolicyClientRuntime reports only this caller's exact canonical session. Active
// attempts are distinct from the last successful completion; failed retries do
// not become the account reported between requests. Reads do not refresh TTLs.
func (m *Manager) PolicyClientRuntime(caller, sessionID string, now time.Time) (map[string]int, string, time.Time) {
	accounts, last, at := m.PolicyClientRuntimeSnapshot(caller, sessionID, now)
	active := make(map[string]int, len(accounts))
	for id, account := range accounts {
		active[id] = account.ActiveRequests
	}
	if last != nil {
		return active, last.CredentialID, at
	}
	return active, "", time.Time{}
}

// PolicyClientRuntimeSnapshot retains ownership proof at the reporting boundary.
func (m *Manager) PolicyClientRuntimeSnapshot(caller, sessionID string, now time.Time) (map[string]PolicyClientAccount, *PolicyClientAccount, time.Time) {
	active := make(map[string]PolicyClientAccount)
	if m == nil || caller == "" || sessionID == "" {
		return active, nil, time.Time{}
	}
	key := policyClientKey{caller, sessionID}
	var selections []policyClientSelection
	m.policyRuntimeMu.Lock()
	for lease := range m.policyPending {
		if lease.clientKey == key && lease.authID != "" && lease.clientSelection.authID == lease.authID {
			selections = append(selections, lease.clientSelection)
		}
	}
	last, ok := m.policyClientHistory[key]
	m.policyRuntimeMu.Unlock()
	for _, selection := range selections {
		if a, exists := m.GetByID(selection.authID); exists && a.RegistrationEpoch == selection.epoch && policyClientIdentity(a) == selection.identity {
			active[selection.authID] = policyClientAccount(a, active[selection.authID].ActiveRequests+1)
		}
	}
	if ok && !last.at.After(now) && now.Sub(last.at) <= policyClientHistoryTTL {
		if a, exists := m.GetByID(last.authID); exists && a.RegistrationEpoch == last.epoch && policyClientIdentity(a) == last.identity {
			view := policyClientAccount(a, active[last.authID].ActiveRequests)
			return active, &view, last.at
		}
	}
	return active, nil, time.Time{}
}
