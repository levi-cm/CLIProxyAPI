package auth

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

// Persisted split overrides retain their participants and absolute expiry. Legacy
// percentages no longer allocate requests: all manual spreading uses live leases.
func (s *EarliestDeadlineSelector) pickSplit(provider string, opts cliproxyexecutor.Options, eligible []*Auth, evaluations map[string]accountpolicy.Evaluation) *Auth {
	id, members := s.routingSplit(provider)
	if id == "" {
		return nil
	}
	return s.pickPressureMembers(members, opts, eligible, evaluations)
}
