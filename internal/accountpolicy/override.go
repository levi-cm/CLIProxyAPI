package accountpolicy

import (
	"github.com/google/uuid"
	"time"
)

// RoutingOverride is the global operational view. Logical identity is stored
// separately in the policy journal and is never returned to integration clients.
type RoutingOverride struct {
	SchemaVersion     int        `json:"schema_version"`
	ID                string     `json:"id"`
	Revision          uint64     `json:"revision"`
	CredentialID      string     `json:"credential_id"`
	Provider          string     `json:"provider"`
	StartedAt         time.Time  `json:"started_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	State             string     `json:"state"`
	TerminationReason string     `json:"termination_reason,omitempty"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
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
	if hours < 1 || hours > 24 {
		return RoutingOverride{}, policyError("invalid_duration", "duration must be 1 through 24 whole hours")
	}
	identity, err := s.identity(id)
	if err != nil {
		return RoutingOverride{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := s.state.Settings
	if s.state.Override.Revision != revision {
		return RoutingOverride{}, policyError("conflict", "override revision changed")
	}
	if !settings.Enabled {
		return RoutingOverride{}, policyError("disabled", "account policy routing is disabled")
	}
	if settings.ReadOnly || s.replica {
		return RoutingOverride{}, policyError("read_only", "policy is read only")
	}
	if s.stateDir == "" {
		return RoutingOverride{}, policyError("persistence", "override requires durable state")
	}
	snapshot, ok := s.state.Snapshots[id]
	now := s.now()
	if identity.Provider != "codex" || !ok || !sameOwnership(identity, snapshot.Identity) || identity.AccountID == "" {
		return RoutingOverride{}, policyError("unknown_account", "known supported account identity required")
	}
	e := overrideEvaluation(snapshot, settings, now)
	if !e.Eligible || !e.Known || !fresh(snapshot.ObservedAt, now, settings.FreshnessSeconds) || snapshot.LastError != "" {
		return RoutingOverride{}, policyError("unavailable", "fresh eligible account evidence required")
	}
	before := clone(s.state)
	s.state.Override = RoutingOverride{SchemaVersion: 1, ID: uuid.NewString(), Revision: revision + 1, CredentialID: id, Provider: identity.Provider, StartedAt: now, ExpiresAt: now.Add(time.Duration(hours) * time.Hour), State: "active"}
	s.state.OverrideIdentity = identity
	s.state.Settings.ForceAccount = ""
	if err = s.persistLocked(before); err != nil {
		return RoutingOverride{}, err
	}
	return clone(s.state.Override), nil
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
func (s *Service) overrideTerminationLocked(ids []Identity, temporary bool) string {
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
	found := false
	for _, id := range ids {
		if sameOwnership(id, s.state.OverrideIdentity) {
			found = true
			break
		}
	}
	if !found {
		return "unavailable"
	}
	snapshot, ok := s.state.Snapshots[o.CredentialID]
	if !ok || !sameOwnership(snapshot.Identity, s.state.OverrideIdentity) {
		return "unavailable"
	}
	if !temporary && (snapshot.Status == "authentication_unavailable" || snapshot.Status == "disabled" || snapshot.Status == "authentication_blocked") {
		return "unavailable"
	}
	for _, b := range snapshot.Buckets {
		if applicable(b, "") && (b.UsedPercent >= 100 || (b.Allowed != nil && !*b.Allowed)) {
			return "exhausted"
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
	active := s.state.Override.State == "active" && s.state.Override.Provider == provider
	s.mu.RUnlock()
	if !active {
		return ""
	}
	ids, temporary := s.overrideIdentities()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.Override.Provider != provider || s.state.Override.State != "active" || s.overrideTerminationLocked(ids, temporary) != "" {
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
	if s.state.Override.State != "active" || !sameOwnership(s.state.OverrideIdentity, identity) {
		return nil
	}
	before := clone(s.state)
	s.endOverrideLocked("exhausted")
	return s.persistLocked(before)
}

func (s *Service) overrideIdentities() ([]Identity, bool) {
	ids := s.opts.Accounts()
	if s.opts.OverrideAccountState == nil {
		return ids, false
	}
	s.mu.RLock()
	selected := s.state.Override.CredentialID
	s.mu.RUnlock()
	if selected == "" {
		return ids, false
	}
	identity, state := s.opts.OverrideAccountState(selected)
	// The host's existence view includes temporarily ineligible credentials. It
	// must not be inferred from the eligible routing pool during token refresh.
	filtered := make([]Identity, 0, len(ids)+1)
	for _, id := range ids {
		if id.CredentialID != selected {
			filtered = append(filtered, id)
		}
	}
	if state == "available" || state == "temporary" {
		filtered = append(filtered, identity)
	}
	return filtered, state == "temporary"
}
