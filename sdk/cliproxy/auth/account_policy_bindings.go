package auth

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicybindings"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	log "github.com/sirupsen/logrus"
)

const durableBindingSelectionKey = "account_policy_durable_selection"
const durableBindingExpectedKey = "account_policy_durable_expected"

type durableSelection struct {
	key      accountpolicybindings.Key
	owner    accountpolicybindings.Owner
	expected *accountpolicybindings.Owner
	epoch    uint64
}

// SetBindingAuthResolver supplies a fresh registered credential at completion.
// Process-local registration epochs guard late results; they are never persisted.
func (s *EarliestDeadlineSelector) SetBindingAuthResolver(resolve func(string) (*Auth, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindingAuth = resolve
}

func (s *EarliestDeadlineSelector) durableStore() *accountpolicybindings.Store {
	settings := s.policy.Settings()
	if settings.StateDir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindingStore == nil {
		s.bindingStore = accountpolicybindings.New(settings.StateDir)
	}
	return s.bindingStore
}

func durableOwner(a *Auth) (accountpolicybindings.Owner, bool) {
	id, err := PolicyAccountIdentity(a)
	if err != nil {
		return accountpolicybindings.Owner{}, false
	}
	return accountpolicybindings.Owner{CredentialID: id.CredentialID, AccountID: id.AccountID, WorkspaceID: id.WorkspaceID}, true
}

func (s *EarliestDeadlineSelector) restoreDurableBindings(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) error {
	if (provider != "codex" && provider != "mixed") || policyClientScope(opts) == "" {
		return nil
	}
	store := s.durableStore()
	if store == nil {
		return nil
	}
	primary, parent := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primary == "" {
		return nil
	} // Never infer a lost owner from prompt similarity.
	primary = session.BoundSessionIdentity(primary)
	model = canonicalModelKey(model)
	for _, id := range []string{primary, session.BoundSessionIdentity(parent)} {
		if id == "" {
			continue
		}
		// The manager's multi-provider route namespace is "mixed". Ownership
		// records describe the actual selected provider, never that route label.
		key := accountpolicybindings.NewKey(policyClientScope(opts), "codex", id, model)
		owner, found, err := store.Lookup(key)
		if err != nil {
			return &Error{Code: "account_policy_binding_state_unavailable", Message: "conversation ownership state is unavailable; no account can be inferred", HTTPStatus: http.StatusServiceUnavailable}
		}
		if id == primary {
			var expected *accountpolicybindings.Owner
			if found {
				copy := owner
				expected = &copy
			}
			opts.Metadata[durableBindingExpectedKey] = expected
		}
		if !found {
			continue
		}
		var auth *Auth
		for _, candidate := range auths {
			if candidate.ID == owner.CredentialID {
				auth = candidate
				break
			}
		}
		available := auth != nil
		if !available {
			s.mu.Lock()
			resolve := s.bindingAuth
			s.mu.Unlock()
			if resolve != nil {
				auth, _ = resolve(owner.CredentialID)
			}
		}
		identity, valid := durableOwner(auth)
		snapshot, exists := s.policy.Snapshot(owner.CredentialID)
		if !valid || identity != owner || !exists || snapshot.Identity.CredentialID != owner.CredentialID || snapshot.Identity.Provider != "codex" || snapshot.Identity.AccountID != owner.AccountID || snapshot.Identity.WorkspaceID != owner.WorkspaceID {
			return &Error{Code: "account_policy_owner_mismatch", Message: "saved conversation owner is unavailable or its credential/account/workspace identity changed", HTTPStatus: http.StatusConflict}
		}
		if pinned := pinnedAuthIDFromMetadata(opts.Metadata); id == primary && pinned != "" && pinned != owner.CredentialID {
			return &Error{Code: "account_policy_owner_mismatch", Message: "explicit account conflicts with the saved conversation owner", HTTPStatus: http.StatusConflict}
		}
		cacheKey := policyAffinityNamespace(provider, opts) + "::" + id + "::" + model
		if !available {
			s.mu.Lock()
			idle := s.idle
			s.mu.Unlock()
			if id == primary && parent == "" && policyRequestReplayable(ctx, opts) && idle != nil && idle(owner.CredentialID) {
				s.affinity.cache.CompareAndDelete(cacheKey, owner.CredentialID)
				return nil // Preserve existing migration of self-contained work only.
			}
			return &Error{Code: "unsafe_affinity_unavailable", Message: "saved conversation owner is unavailable; account-specific continuation cannot move", HTTPStatus: http.StatusConflict}
		}
		cached, exists := s.affinity.cache.Get(cacheKey)
		if exists && cached != owner.CredentialID {
			return &Error{Code: "account_policy_owner_conflict", Message: "local and durable conversation owners conflict; continuation cannot be routed safely", HTTPStatus: http.StatusConflict}
		}
		if !exists {
			s.affinity.cache.Set(cacheKey, auth.ID)
			s.mu.Lock()
			s.bindings[cacheKey] = policyBinding{authID: auth.ID, epoch: auth.RegistrationEpoch, account: owner.AccountID, workspace: owner.WorkspaceID, seen: s.now()}
			s.mu.Unlock()
		}
		if id == primary {
			return nil // A known child owns its continuation independently of its parent.
		}
	}
	return nil
}

func (s *EarliestDeadlineSelector) captureDurableBinding(provider, model string, opts cliproxyexecutor.Options, a *Auth) {
	if a.Provider != "codex" || policyClientScope(opts) == "" || s.durableStore() == nil {
		return
	}
	primary, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	owner, valid := durableOwner(a)
	if primary == "" || !valid {
		return
	}
	expected, _ := opts.Metadata[durableBindingExpectedKey].(*accountpolicybindings.Owner)
	opts.Metadata[durableBindingSelectionKey] = durableSelection{key: accountpolicybindings.NewKey(policyClientScope(opts), a.Provider, session.BoundSessionIdentity(primary), canonicalModelKey(model)), owner: owner, expected: expected, epoch: a.RegistrationEpoch}
}

func (s *EarliestDeadlineSelector) persistDurableBinding(result Result) {
	if !result.Success || result.SkipQuotaObservation || result.Provider != "codex" {
		return
	}
	selection, found := result.Options.Metadata[durableBindingSelectionKey].(durableSelection)
	if !found || selection.owner.CredentialID != result.AuthID {
		return
	}
	s.mu.Lock()
	resolve := s.bindingAuth
	s.mu.Unlock()
	if resolve == nil {
		return
	}
	a, exists := resolve(result.AuthID)
	owner, valid := durableOwner(a)
	if !exists || !valid || owner != selection.owner || a.RegistrationEpoch != selection.epoch {
		return
	}
	store := s.durableStore()
	if store == nil {
		return
	}
	if err := store.Put(selection.key, selection.owner, selection.expected); err != nil {
		log.WithField("component", "account-policy-bindings").Error("conversation owner was not persisted; resume after restart may require recovery")
	}
}
