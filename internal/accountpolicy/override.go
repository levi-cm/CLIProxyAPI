package accountpolicy

import "time"

// RoutingOverride is the global operational view. Logical identity is stored
// separately in the policy journal and is never returned to integration clients.
type RoutingOverride struct {
	SchemaVersion     int            `json:"schema_version"`
	Mode              string         `json:"mode,omitempty"`
	Accounts          []RoutingShare `json:"accounts,omitempty"`
	ID                string         `json:"id"`
	Revision          uint64         `json:"revision"`
	CredentialID      string         `json:"credential_id"`
	Provider          string         `json:"provider"`
	StartedAt         time.Time      `json:"started_at"`
	ExpiresAt         time.Time      `json:"expires_at"`
	State             string         `json:"state"`
	TerminationReason string         `json:"termination_reason,omitempty"`
	EndedAt           *time.Time     `json:"ended_at,omitempty"`
}

func (s *Service) RoutingOverride() RoutingOverride {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o := clone(s.state.Override)
	o.SchemaVersion = 1
	if o.State == "" {
		o.State = "inactive"
	}
	return o
}
func overrideEvaluation(snapshot Snapshot, settings Settings, now time.Time) Evaluation {
	settings.ForceAccount = ""
	control := settings.Accounts[snapshot.Identity.CredentialID]
	control.ReservePercent = 0
	settings.Accounts = clone(settings.Accounts)
	if settings.Accounts == nil {
		settings.Accounts = map[string]AccountControl{}
	}
	settings.Accounts[snapshot.Identity.CredentialID] = control
	return Evaluate(snapshot, "", settings, now)
}
func (s *Service) ReplaceOverride(id string, hours int, revision uint64) (RoutingOverride, error) {
	return s.replaceRoutingOverride("single", []RoutingShare{{CredentialID: id, Percent: 100}}, hours, revision)
}
func (s *Service) endOverrideLocked(reason string) {
	o := &s.state.Override
	if o.State != "active" {
		return
	}
	now := s.now()
	o.State = reason
	o.TerminationReason = reason
	o.EndedAt = &now
	o.Revision++
}
func (s *Service) CancelOverride(revision uint64) (RoutingOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Settings.ReadOnly || s.replica {
		return RoutingOverride{}, policyError("read_only", "policy is read only")
	}
	if s.state.Override.Revision != revision {
		return RoutingOverride{}, policyError("conflict", "override revision changed")
	}
	if s.state.Override.State != "active" {
		return clone(s.state.Override), nil
	}
	before := clone(s.state)
	s.endOverrideLocked("cancelled")
	if err := s.persistLocked(before); err != nil {
		return RoutingOverride{}, err
	}
	return clone(s.state.Override), nil
}
func (s *Service) overrideTerminationLocked(ids []Identity, temporary map[string]bool) string {
	o := s.state.Override
	if o.State != "active" {
		return ""
	}
	if !s.state.Settings.Enabled {
		return "policy-disabled"
	}
	if !o.ExpiresAt.After(s.now()) {
		return "expired"
	}
	members := s.overrideMembersLocked()
	if len(members) == 0 {
		return "unavailable"
	}
	for _, member := range members {
		found := false
		for _, id := range ids {
			if sameOwnership(id, member) {
				found = true
				break
			}
		}
		if !found {
			return "unavailable"
		}
		snapshot, ok := s.state.Snapshots[member.CredentialID]
		if !ok || !sameOwnership(snapshot.Identity, member) {
			return "unavailable"
		}
		if !temporary[member.CredentialID] && (snapshot.Status == "authentication_unavailable" || snapshot.Status == "disabled" || snapshot.Status == "authentication_blocked") {
			return "unavailable"
		}
		for _, b := range snapshot.Buckets {
			if applicable(b, "") && (b.UsedPercent >= 100 || (b.Allowed != nil && !*b.Allowed)) {
				return "exhausted"
			}
		}
	}
	return ""
}
func (s *Service) MaintainOverride() error {
	s.mu.RLock()
	active := s.state.Override.State == "active"
	s.mu.RUnlock()
	if !active {
		return nil
	}
	ids, temporary := s.overrideIdentities()
	s.mu.Lock()
	defer s.mu.Unlock()
	reason := s.overrideTerminationLocked(ids, temporary)
	if reason == "" {
		return nil
	}
	before := clone(s.state)
	s.endOverrideLocked(reason)
	return s.persistLocked(before)
}

// PreferredAccount checks absolute expiry at selection time without archive I/O,
// provider calls, or waiting for maintenance. Hard eligibility is still applied
// by the selector; expiry never erases or migrates durable conversation owners.
func (s *Service) PreferredAccount(provider string) string {
	s.mu.RLock()
	active := s.state.Override.State == "active" && s.state.Override.Provider == provider && s.state.Override.Mode != "split"
	s.mu.RUnlock()
	if !active {
		return ""
	}
	ids, temporary := s.overrideIdentities()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.Override.Provider != provider || s.state.Override.Mode == "split" || s.state.Override.State != "active" || s.overrideTerminationLocked(ids, temporary) != "" {
		return ""
	}
	return s.state.Override.CredentialID
}

// ConfirmOverrideExhaustion accepts a host-validated ordinary quota signal for
// the exact selected logical account. It never changes active request leases.
func (s *Service) ConfirmOverrideExhaustion(identity Identity) error {
	current, err := s.identity(identity.CredentialID)
	if err != nil || !sameOwnership(current, identity) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Override.State != "active" {
		return nil
	}
	participant := false
	for _, member := range s.overrideMembersLocked() {
		if sameOwnership(member, identity) {
			participant = true
			break
		}
	}
	if !participant {
		return nil
	}
	before := clone(s.state)
	s.endOverrideLocked("exhausted")
	return s.persistLocked(before)
}

func (s *Service) overrideIdentities() ([]Identity, map[string]bool) {
	ids := s.opts.Accounts()
	temporary := map[string]bool{}
	if s.opts.OverrideAccountState == nil {
		return ids, temporary
	}
	s.mu.RLock()
	members := clone(s.overrideMembersLocked())
	s.mu.RUnlock()
	for _, member := range members {
		identity, state := s.opts.OverrideAccountState(member.CredentialID)
		filtered := make([]Identity, 0, len(ids)+1)
		for _, id := range ids {
			if id.CredentialID != member.CredentialID {
				filtered = append(filtered, id)
			}
		}
		if state == "available" || state == "temporary" {
			filtered = append(filtered, identity)
		}
		temporary[member.CredentialID] = state == "temporary"
		ids = filtered
	}
	return ids, temporary
}
