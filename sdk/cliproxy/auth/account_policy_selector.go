package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// AccountPolicySource supplies immutable observations; selection performs no I/O.
type AccountPolicySource interface {
	Settings() accountpolicy.Settings
	Snapshot(string) (accountpolicy.Snapshot, bool)
	RecordDecision(accountpolicy.Decision)
}

type policyBinding struct {
	authID             string
	epoch              uint64
	account, workspace string
	seen               time.Time
}

// EarliestDeadlineSelector composes deadline ranking with the existing session
// resolver/cache. The optional clock should be shared with the observation service.
type EarliestDeadlineSelector struct {
	policy           AccountPolicySource
	fallback         Selector
	disabledFallback Selector
	affinity         *SessionAffinitySelector
	now              func() time.Time
	mu               sync.Mutex
	idle             func(string) bool
	bindings         map[string]policyBinding
}

func NewEarliestDeadlineSelector(policy AccountPolicySource, fallback Selector, clocks ...func() time.Time) *EarliestDeadlineSelector {
	if fallback == nil {
		fallback = &RoundRobinSelector{}
	}
	s := &EarliestDeadlineSelector{policy: policy, fallback: fallback, now: time.Now, bindings: make(map[string]policyBinding)}
	if len(clocks) > 0 && clocks[0] != nil {
		s.now = clocks[0]
	}
	s.affinity = NewSessionAffinitySelector(&deadlineRankingSelector{owner: s})
	return s
}

// SetIdleCheck installs the host's real execution accounting. Without it, an
// established session cannot migrate because a completed boundary is unproven.
func (s *EarliestDeadlineSelector) SetIdleCheck(check func(string) bool) {
	s.mu.Lock()
	s.idle = check
	s.mu.Unlock()
}

// SetDisabledFallback preserves the host's configured affinity when the optional
// policy is switched off through management settings.
func (s *EarliestDeadlineSelector) SetDisabledFallback(fallback Selector) {
	s.mu.Lock()
	s.disabledFallback = fallback
	s.mu.Unlock()
}

// OwnsSelector transfers the configured fallback's lifetime when the host wraps
// an existing affinity selector, preserving its current bindings.
func (s *EarliestDeadlineSelector) OwnsSelector(selector Selector) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return isSameSelector(selector, s.disabledFallback) || isSameSelector(selector, s.fallback)
}
func (s *EarliestDeadlineSelector) SelectorWantsAcrossPriorities() bool { return true }
func (s *EarliestDeadlineSelector) Stop() {
	s.affinity.Stop()
	s.mu.Lock()
	fallback := s.disabledFallback
	s.mu.Unlock()
	if stop, ok := fallback.(StoppableSelector); ok {
		stop.Stop()
	}
}
func (s *EarliestDeadlineSelector) OnResult(result Result) {
	if s.policy == nil || !s.policy.Settings().Enabled {
		s.mu.Lock()
		fallback := s.disabledFallback
		s.mu.Unlock()
		if fallback == nil {
			fallback = s.fallback
		}
		if observer, ok := fallback.(interface{ OnResult(Result) }); ok {
			observer.OnResult(result)
		}
		return
	}
	// A failed account remains the thread's owner until a subsequent request
	// proves that failover can preserve its conversation state.
	if result.Success {
		s.affinity.OnResult(result)
	}
}
func (s *EarliestDeadlineSelector) InvalidateAuth(id string) {
	s.affinity.InvalidateAuth(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, b := range s.bindings {
		if b.authID == id {
			delete(s.bindings, key)
		}
	}
}

func (s *EarliestDeadlineSelector) ActiveBindings(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	count := 0
	for key, b := range s.bindings {
		if now.Sub(b.seen) > time.Hour {
			delete(s.bindings, key)
		} else if b.authID == id {
			count++
		}
	}
	return count
}

// A public lookup without client identity cannot disambiguate isolated bindings.
func (s *EarliestDeadlineSelector) LookupAffinity(_, _, _ string, _ ...func(string) bool) (string, string) {
	return "", "unsupported"
}

func policyClientScope(opts cliproxyexecutor.Options) string {
	caller, _ := opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey].(string)
	if caller == "" {
		caller = opts.Headers.Get("Authorization")
	}
	if caller == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(caller))
	return hex.EncodeToString(digest[:])
}

func policyAffinityNamespace(provider string, opts cliproxyexecutor.Options) string {
	scope := policyClientScope(opts)
	if scope == "" {
		return provider + "::unidentified-client"
	}
	return provider + "::client-" + scope
}

func policyBindingKey(provider, model string, opts cliproxyexecutor.Options) string {
	session := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if session == "" {
		return ""
	}
	return policyAffinityNamespace(provider, opts) + "::" + session + "::" + canonicalModelKey(model)
}

func (s *EarliestDeadlineSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if s.policy == nil || !s.policy.Settings().Enabled {
		s.mu.Lock()
		fallback := s.disabledFallback
		s.mu.Unlock()
		if fallback == nil {
			fallback = s.fallback
		}
		return fallback.Pick(ctx, provider, model, opts, auths)
	}
	available, err := getSelectorAvailableAuthsAcrossPriorities(ctx, auths, provider, model, s.now())
	if err != nil {
		return nil, err
	}
	eligible, _, _ := s.evaluate(provider, model, available)
	if len(eligible) == 0 {
		return nil, &Error{Code: "account_policy_unavailable", Message: "no account satisfies account policy", HTTPStatus: 503}
	}
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	// Missing client identity cannot safely share persistent conversation state.
	if policyClientScope(opts) == "" {
		if !policyRequestReplayable(ctx, opts) && ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata) != "" {
			return nil, &Error{Code: "account_policy_client_unknown", Message: "client identity is required for account-specific continuation", HTTPStatus: 409}
		}
		return (&deadlineRankingSelector{owner: s}).Pick(ctx, provider, model, opts, eligible)
	}
	selected, errPick := s.affinity.Pick(ctx, provider, model, opts, eligible)
	if errPick != nil || selected == nil {
		return selected, errPick
	}
	key := policyBindingKey(provider, model, opts)
	if key != "" {
		snapshot, _ := s.policy.Snapshot(selected.ID)
		s.mu.Lock()
		// Bounded by the session cache's capacity and TTL, without storing prompts.
		if len(s.bindings) >= defaultMaxSessionEntries {
			for staleKey, b := range s.bindings {
				if s.now().Sub(b.seen) > time.Hour {
					delete(s.bindings, staleKey)
				}
			}
		}
		if len(s.bindings) < defaultMaxSessionEntries {
			s.bindings[key] = policyBinding{authID: selected.ID, epoch: selected.RegistrationEpoch, account: snapshot.Identity.AccountID, workspace: snapshot.Identity.WorkspaceID, seen: s.now()}
		}
		s.mu.Unlock()
	}
	return selected, nil
}

func (s *EarliestDeadlineSelector) evaluate(provider, model string, auths []*Auth) ([]*Auth, map[string]accountpolicy.Evaluation, bool) {
	settings := s.policy.Settings()
	now := s.now()
	unknown := false
	eligible := make([]*Auth, 0, len(auths))
	evaluations := make(map[string]accountpolicy.Evaluation, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		snapshot, ok := s.policy.Snapshot(a.ID)
		if !ok {
			snapshot.Identity = accountpolicy.Identity{CredentialID: a.ID, Provider: a.Provider}
		}
		if snapshot.Identity.CredentialID != a.ID || snapshot.Identity.Provider != a.Provider {
			continue
		}
		evaluation := accountpolicy.Evaluate(snapshot, model, settings, now)
		if !evaluation.Eligible {
			continue
		}
		eligible = append(eligible, a)
		evaluations[a.ID] = evaluation
		unknown = unknown || !evaluation.Known
	}
	return eligible, evaluations, unknown
}

func (s *EarliestDeadlineSelector) decision(a *Auth, provider, model, reason string, e accountpolicy.Evaluation, fallback bool) {
	if a == nil {
		return
	}
	s.policy.RecordDecision(accountpolicy.Decision{CredentialID: a.ID, Provider: a.Provider, Model: model, Reason: reason, Deadline: e.Deadline, At: s.now(), Fallback: fallback})
}

type deadlineRankingSelector struct{ owner *EarliestDeadlineSelector }

func (r *deadlineRankingSelector) SelectorWantsAcrossPriorities() bool { return true }
func (r *deadlineRankingSelector) AffinityNamespace(provider string, opts cliproxyexecutor.Options) string {
	return policyAffinityNamespace(provider, opts)
}

func (r *deadlineRankingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	s := r.owner
	eligible, evaluations, unknown := s.evaluate(provider, model, auths)
	if len(eligible) == 0 {
		return nil, &Error{Code: "account_policy_unavailable", Message: "no account satisfies account policy", HTTPStatus: 503}
	}
	if unknown {
		a, err := s.fallback.Pick(ctx, provider, model, opts, eligible)
		s.decision(a, provider, model, "missing_or_stale_evidence", accountpolicy.Evaluation{}, true)
		return a, err
	}
	sort.Slice(eligible, func(i, j int) bool {
		left, right := evaluations[eligible[i].ID].Deadline, evaluations[eligible[j].ID].Deadline
		if !left.Equal(right) {
			return left.Before(right)
		}
		if authPriority(eligible[i]) != authPriority(eligible[j]) {
			return authPriority(eligible[i]) > authPriority(eligible[j])
		}
		return eligible[i].ID < eligible[j].ID
	})
	a := eligible[0]
	e := evaluations[a.ID]
	s.decision(a, provider, model, e.Reason, e, false)
	return a, nil
}

func (r *deadlineRankingSelector) PickUnavailable(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, id string) error {
	s := r.owner
	s.mu.Lock()
	idle := s.idle
	s.mu.Unlock()
	if !policyRequestReplayable(ctx, opts) || idle == nil || !idle(id) {
		s.policy.RecordDecision(accountpolicy.Decision{CredentialID: id, Provider: provider, Model: model, Reason: "unsafe_affinity_unavailable", At: s.now()})
		return &Error{Code: "unsafe_affinity_unavailable", Message: "account-specific continuation cannot move to another account", HTTPStatus: 409}
	}
	return nil
}

func (r *deadlineRankingSelector) PickBound(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, bound *Auth, auths []*Auth) (*Auth, error) {
	s := r.owner
	_, evaluations, unknown := s.evaluate(provider, model, auths)
	e := evaluations[bound.ID]
	key := policyBindingKey(provider, model, opts)
	s.mu.Lock()
	binding, exists := s.bindings[key]
	idle := s.idle
	s.mu.Unlock()
	snapshot, _ := s.policy.Snapshot(bound.ID)
	if exists && (binding.epoch != bound.RegistrationEpoch || binding.account != snapshot.Identity.AccountID || binding.workspace != snapshot.Identity.WorkspaceID) {
		if err := r.PickUnavailable(ctx, provider, model, opts, bound.ID); err != nil {
			return nil, err
		}
		return r.Pick(ctx, provider, model, opts, auths)
	}
	reason := "strict_affinity"
	if s.policy.Settings().Affinity == "deadline_at_boundary" {
		reason = "unsafe_or_unknown_continuation"
		parent, _ := opts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey].(string)
		if parent == "" && !unknown && policyRequestReplayable(ctx, opts) && idle != nil && idle(bound.ID) {
			var best *Auth
			for _, candidate := range auths {
				ce := evaluations[candidate.ID]
				if best == nil || ce.Deadline.Before(evaluations[best.ID].Deadline) {
					best = candidate
				}
			}
			if best != nil && best.ID != bound.ID && evaluations[best.ID].Deadline.Before(e.Deadline) {
				be := evaluations[best.ID]
				s.decision(best, provider, model, "deadline_migration_from_"+bound.ID, be, false)
				return best, nil
			}
			reason = "retained_affinity"
		}
	}
	s.decision(bound, provider, model, reason, e, false)
	return bound, nil
}

// Replayability is a positive check of self-contained input, plus explicit
// rejection of account-local lineage anywhere in the payload. No replay occurs here.
func policyRequestReplayable(ctx context.Context, opts cliproxyexecutor.Options) bool {
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return false
	}
	for _, key := range []string{cliproxyexecutor.ExecutionSessionMetadataKey, "turn_state", "previous_response_id", "encrypted_reasoning"} {
		if v, ok := opts.Metadata[key]; ok && v != nil && v != "" {
			return false
		}
	}
	var root map[string]any
	if json.Unmarshal(opts.OriginalRequest, &root) != nil {
		return false
	}
	var unsafe func(any) bool
	unsafe = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				switch strings.ToLower(key) {
				case "previous_response_id", "encrypted_content", "encrypted_reasoning", "turn_state", "conversation_id":
					if item != nil && item != "" {
						return true
					}
				}
				if key == "type" && (item == "item_reference" || item == "compaction") {
					return true
				}
				if key == "conversation" && item != nil {
					return true
				}
				if unsafe(item) {
					return true
				}
			}
		case []any:
			for _, item := range v {
				if unsafe(item) {
					return true
				}
			}
		}
		return false
	}
	if unsafe(root) {
		return false
	}
	if input, ok := root["input"]; ok {
		switch v := input.(type) {
		case string:
			return strings.TrimSpace(v) != ""
		case []any:
			return len(v) > 0
		}
	}
	if messages, ok := root["messages"].([]any); ok {
		return len(messages) > 0
	}
	return false
}
