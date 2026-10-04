package auth

import (
	"context"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SelectorAcrossPriorities opts a dynamic selector into all eligible priority tiers.
type SelectorAcrossPriorities interface {
	SelectorWantsAcrossPriorities() bool
}

func selectorWantsAcrossPriorities(selector Selector) bool {
	opt, ok := selector.(SelectorAcrossPriorities)
	return ok && opt.SelectorWantsAcrossPriorities()
}

func (s *SessionAffinitySelector) SelectorWantsAcrossPriorities() bool {
	return s != nil && selectorWantsAcrossPriorities(s.fallback)
}

func (s *SessionAffinitySelector) fallbackCandidates(available []*Auth) []*Auth {
	if selectorWantsAcrossPriorities(s.fallback) {
		return available
	}
	return highestPriorityAuths(available)
}

// BoundAffinitySelector may retain or safely replace an eligible binding.
// It must never return another account for an unsafe continuation.
type BoundAffinitySelector interface {
	PickBound(context.Context, string, string, cliproxyexecutor.Options, *Auth, []*Auth) (*Auth, error)
}

func (s *SessionAffinitySelector) pickBound(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, bound *Auth, available []*Auth) (*Auth, error) {
	if policy, ok := s.fallback.(BoundAffinitySelector); ok {
		return policy.PickBound(ctx, provider, model, opts, bound, available)
	}
	return bound, nil
}

func (s *SessionAffinitySelector) pickParent(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, parent string, bound *Auth, available []*Auth) (*Auth, error) {
	if policy, ok := s.fallback.(interface {
		PickParent(context.Context, string, string, cliproxyexecutor.Options, string, *Auth, []*Auth) (*Auth, error)
	}); ok {
		return policy.PickParent(ctx, provider, model, opts, parent, bound, available)
	}
	return bound, nil
}

func (s *SessionAffinitySelector) affinityNamespace(provider string, opts cliproxyexecutor.Options) string {
	if scoped, ok := s.fallback.(interface {
		AffinityNamespace(string, cliproxyexecutor.Options) string
	}); ok {
		return scoped.AffinityNamespace(provider, opts)
	}
	return provider
}

func (s *SessionAffinitySelector) checkUnavailableAffinity(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, id string) error {
	if policy, ok := s.fallback.(interface {
		PickUnavailable(context.Context, string, string, cliproxyexecutor.Options, string) error
	}); ok {
		return policy.PickUnavailable(ctx, provider, model, opts, id)
	}
	return nil
}
