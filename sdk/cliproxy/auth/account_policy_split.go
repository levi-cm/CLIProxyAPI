package auth

import (
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

func (s *EarliestDeadlineSelector) routingSplit(provider string) (string, []accountpolicy.RoutingShare) {
	if source, ok := s.policy.(interface {
		RoutingSplit(string) (string, []accountpolicy.RoutingShare)
	}); ok {
		id, shares := source.RoutingSplit(provider)
		if id != "" {
			return id, shares
		}
	}
	return "", nil
}

// pickSplit advances only an eligible routing decision. Local descriptors carry
// the configured percentages; registered Auth objects and weights stay intact.
// One accumulator spans models and callers, and replacement resets its credits.
func (s *EarliestDeadlineSelector) pickSplit(provider string, eligible []*Auth) *Auth {
	id, shares := s.routingSplit(provider)
	if id == "" {
		return nil
	}
	originals := make(map[string]*Auth, len(eligible))
	for _, a := range eligible {
		if a != nil {
			originals[a.ID] = a
		}
	}
	weighted := make([]*Auth, 0, len(shares))
	for _, share := range shares {
		if originals[share.CredentialID] != nil && share.Percent > 0 {
			weighted = append(weighted, &Auth{ID: share.CredentialID, Attributes: map[string]string{AttributeWeight: strconv.Itoa(share.Percent)}})
		}
	}
	if len(weighted) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.splitID != id || s.splitState == nil {
		s.splitID, s.splitState = id, &smoothWeightedState{}
	}
	s.splitState.prepare(authWeightVector(weighted))
	selected := pickSmoothWeightedAuth(weighted, s.splitState.current)
	if selected == nil {
		return nil
	}
	return originals[selected.ID]
}
