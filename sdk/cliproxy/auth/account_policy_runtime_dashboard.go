package auth

import "time"

// PolicyRuntimeSnapshot samples all requested credentials without invoking the
// legacy per-account binding observer, which may expire selector-owned state.
// A custom selector with only that legacy observer makes bindings unavailable.
func (m *Manager) PolicyRuntimeSnapshot(ids []string) (map[string]PolicyRuntimeStatus, bool) {
	statuses := make(map[string]PolicyRuntimeStatus, len(ids))
	if m == nil {
		return statuses, false
	}
	m.policyRuntimeMu.Lock()
	for _, id := range ids {
		status := m.policyRuntime[id]
		if status.Transport == "" {
			status.Transport = "unknown"
		}
		if status.LastSelectedAt != nil {
			at := *status.LastSelectedAt
			status.LastSelectedAt = &at
		}
		statuses[id] = status
	}
	m.policyRuntimeMu.Unlock()
	counts, available := policyDashboardBindings(m.Selector())
	for id, status := range statuses {
		status.ActiveBindings = counts[id]
		statuses[id] = status
	}
	return statuses, available
}

func policyDashboardBindings(selector Selector) (map[string]int, bool) {
	if observer, ok := selector.(interface{ ActiveBindingsSnapshot() (map[string]int, bool) }); ok {
		return observer.ActiveBindingsSnapshot()
	}
	_, legacyObserver := selector.(interface{ ActiveBindings(string) int })
	return nil, !legacyObserver
}

// ActiveBindingsSnapshot counts each live policy binding once and deliberately
// leaves expiration and cleanup to selection's existing lifecycle.
func (s *EarliestDeadlineSelector) ActiveBindingsSnapshot() (map[string]int, bool) {
	counts := make(map[string]int)
	if s == nil {
		return counts, true
	}
	if s.policy != nil && !s.policy.Settings().Enabled {
		s.mu.Lock()
		fallback := s.disabledFallback
		if fallback == nil {
			fallback = s.fallback
		}
		s.mu.Unlock()
		return policyDashboardBindings(fallback)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, binding := range s.bindings {
		if now.Sub(binding.seen) <= time.Hour {
			counts[binding.authID]++
		}
	}
	return counts, true
}

// ActiveBindingsSnapshot observes canonical session groups, not their aliases,
// without refreshing TTLs or invoking the cache's mutating lookup operation.
func (s *SessionAffinitySelector) ActiveBindingsSnapshot() (map[string]int, bool) {
	counts := make(map[string]int)
	if s == nil || s.cache == nil {
		return counts, true
	}
	now := time.Now()
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()
	for _, group := range s.cache.groups {
		if now.Before(group.expiresAt) {
			counts[group.authID]++
		}
	}
	return counts, true
}
