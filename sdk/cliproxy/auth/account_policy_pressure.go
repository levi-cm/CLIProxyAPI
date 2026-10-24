package auth

import (
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (s *EarliestDeadlineSelector) routingPressure(provider string) (string, []accountpolicy.RoutingShare) {
	if source, ok := s.policy.(interface {
		RoutingPressure(string) (string, []accountpolicy.RoutingShare)
	}); ok {
		return source.RoutingPressure(provider)
	}
	return "", nil
}

// A last-known renewal orders eligible accounts; it does not make quota evidence
// fresh or authorize a reset. Never project a new cycle from a passed renewal.
func lastKnownWeeklyDeadline(snapshot accountpolicy.Snapshot, model string, now time.Time) time.Time {
	var deadline time.Time
	if snapshot.ObservedAt.IsZero() || snapshot.ObservedAt.After(now) {
		return deadline
	}
	for _, b := range snapshot.Buckets {
		applicable := (b.Scope == "ordinary" || b.Scope == "" || b.Model != "" && b.Scope != "code_review") && (b.Model == "" || b.Model == model)
		if !applicable || b.DurationSeconds != 604800 || b.ObservedAt.IsZero() || b.ObservedAt.After(now) || math.IsNaN(b.UsedPercent) || math.IsInf(b.UsedPercent, 0) || b.UsedPercent < 0 || b.UsedPercent > 100 || !b.ResetAt.After(now) {
			continue
		}
		if deadline.IsZero() || b.ResetAt.Before(deadline) {
			deadline = b.ResetAt
		}
	}
	return deadline
}

func earlierPolicyAuth(a, b *Auth, evaluations map[string]accountpolicy.Evaluation) bool {
	left, right := evaluations[a.ID].Deadline, evaluations[b.ID].Deadline
	if left.IsZero() != right.IsZero() {
		return !left.IsZero()
	}
	if !left.Equal(right) {
		return left.Before(right)
	}
	if authPriority(a) != authPriority(b) {
		return authPriority(a) > authPriority(b)
	}
	return a.ID < b.ID
}

// Only an explicit, unexpired manual override reaches this path. Renewals being
// close never enables spreading; no allowance forecast is inferred from counts.
func (s *EarliestDeadlineSelector) pickPressure(provider string, opts cliproxyexecutor.Options, eligible []*Auth, evaluations map[string]accountpolicy.Evaluation) *Auth {
	id, members := s.routingPressure(provider)
	if id == "" || len(members) != 2 {
		return nil
	}
	return s.pickPressureMembers(members, opts, eligible, evaluations)
}

func (s *EarliestDeadlineSelector) pickPressureMembers(members []accountpolicy.RoutingShare, opts cliproxyexecutor.Options, eligible []*Auth, evaluations map[string]accountpolicy.Evaluation) *Auth {
	if len(members) != 2 {
		return nil
	}
	candidates := make([]*Auth, 0, 2)
	for _, member := range members {
		for _, a := range eligible {
			if a.ID == member.CredentialID {
				if !evaluations[a.ID].Known {
					return nil
				}
				candidates = append(candidates, a)
			}
		}
	}
	if len(candidates) != 2 {
		return nil
	}
	lease, ok := opts.Metadata[policyRequestLeaseMetadataKey].(*policyRequestLease)
	if !ok || lease == nil {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return earlierPolicyAuth(candidates[i], candidates[j], evaluations) })
	return lease.reservePressure(candidates, 0)
}

// Compare and reserve under the same manager lock so a burst sees preceding
// selections even before executor entry. Existing attempt/stream completion owns
// release, and a reset reservation remains exclusive. No cumulative debt exists.
func (l *policyRequestLease) reservePressure(candidates []*Auth, minimumEarlierActive int) *Auth {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.ctx.Err() != nil {
		return nil
	}
	m := l.manager
	m.policyRuntimeMu.Lock()
	defer m.policyRuntimeMu.Unlock()
	available := make([]*Auth, 0, len(candidates))
	for _, a := range candidates {
		if m.policyResetReservations[a.ID] == nil {
			available = append(available, a)
		}
	}
	if minimumEarlierActive > 0 && len(available) != len(candidates) {
		return nil
	}
	if len(available) == 0 {
		return nil
	}
	selected := available[0]
	count := func(id string) int {
		n := m.policyRuntime[id].ActiveRequests
		if l.authID == id {
			n--
		}
		return n
	}
	if len(available) == 2 && count(available[0].ID) >= minimumEarlierActive && count(available[0].ID) > count(available[1].ID)+1 {
		selected = available[1]
	}
	if l.authID != selected.ID {
		if m.policyRuntime == nil {
			m.policyRuntime = make(map[string]PolicyRuntimeStatus)
		}
		if l.authID != "" {
			status := m.policyRuntime[l.authID]
			if status.ActiveRequests > 0 {
				status.ActiveRequests--
			}
			m.policyRuntime[l.authID] = status
		}
		l.authID = selected.ID
		status := m.policyRuntime[selected.ID]
		status.ActiveRequests++
		status.Transport = l.transport
		at := time.Now().UTC()
		status.LastSelectedAt = &at
		m.policyRuntime[selected.ID] = status
	}
	return selected
}
