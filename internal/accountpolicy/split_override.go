package accountpolicy

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// RoutingShare identifies a manual spreading participant. Percent is retained
// for the legacy split wire format; it no longer sets an allocation target.
type RoutingShare struct {
	CredentialID string `json:"credential_id"`
	Percent      int    `json:"percent"`
}

func validSplit(shares []RoutingShare) bool {
	return len(shares) == 2 && strings.TrimSpace(shares[0].CredentialID) != "" && strings.TrimSpace(shares[1].CredentialID) != "" && shares[0].CredentialID != shares[1].CredentialID && shares[0].Percent >= 1 && shares[0].Percent <= 99 && shares[1].Percent >= 1 && shares[1].Percent <= 99 && shares[0].Percent+shares[1].Percent == 100
}

func validStoredRoutingOverride(state storedState) bool {
	o := state.Override
	if o.Mode != "split" && o.Mode != "pressure" {
		return o.Mode == "" || o.Mode == "single"
	}
	if !(o.Mode == "split" && validSplit(o.Accounts) || o.Mode == "pressure" && validPressure(o.Accounts)) || len(state.OverrideIdentities) != 2 || o.Provider != "codex" {
		return false
	}
	for i, member := range state.OverrideIdentities {
		if member.CredentialID != o.Accounts[i].CredentialID || member.Provider != o.Provider || member.AccountID == "" {
			return false
		}
	}
	a, b := state.OverrideIdentities[0], state.OverrideIdentities[1]
	return a.AccountID != b.AccountID || a.WorkspaceID != b.WorkspaceID
}

func (s *Service) overrideMembersLocked() []Identity {
	if s.state.Override.Mode == "split" || s.state.Override.Mode == "pressure" {
		if !validStoredRoutingOverride(s.state) {
			return nil
		}
		return s.state.OverrideIdentities
	}
	return []Identity{s.state.OverrideIdentity}
}

func (s *Service) ReplaceSplitOverride(shares []RoutingShare, hours int, revision uint64) (RoutingOverride, error) {
	if !validSplit(shares) {
		return RoutingOverride{}, policyError("invalid_split", "exactly two distinct accounts with whole percentages 1-99 summing to 100 are required")
	}
	return s.replaceRoutingOverride("split", shares, hours, revision)
}

func (s *Service) replaceRoutingOverride(mode string, shares []RoutingShare, hours int, revision uint64) (RoutingOverride, error) {
	if hours < 1 || hours > 24 {
		return RoutingOverride{}, policyError("invalid_duration", "duration must be 1 through 24 whole hours")
	}
	identities := make([]Identity, 0, len(shares))
	for _, share := range shares {
		identity, err := s.identity(share.CredentialID)
		if err != nil {
			return RoutingOverride{}, err
		}
		identities = append(identities, identity)
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
	now := s.now()
	for _, identity := range identities {
		snapshot, ok := s.state.Snapshots[identity.CredentialID]
		if identity.Provider != "codex" || !ok || !sameOwnership(identity, snapshot.Identity) || identity.AccountID == "" {
			return RoutingOverride{}, policyError("unknown_account", "known supported account identity required")
		}
		e := overrideEvaluation(snapshot, settings, now)
		if !e.Eligible || !e.Known || !fresh(snapshot.ObservedAt, now, settings.FreshnessSeconds) || snapshot.LastError != "" {
			return RoutingOverride{}, policyError("unavailable", "fresh eligible account evidence required")
		}
	}
	before := clone(s.state)
	o := RoutingOverride{SchemaVersion: 1, Mode: mode, ID: uuid.NewString(), Revision: revision + 1, Provider: identities[0].Provider, StartedAt: now, ExpiresAt: now.Add(time.Duration(hours) * time.Hour), State: "active"}
	s.state.OverrideIdentities = nil
	s.state.OverrideIdentity = Identity{}
	if mode == "split" || mode == "pressure" {
		o.Accounts = clone(shares)
		s.state.OverrideIdentities = clone(identities)
	} else {
		o.CredentialID = identities[0].CredentialID
		s.state.OverrideIdentity = identities[0]
	}
	s.state.Override = o
	s.state.Settings.ForceAccount = ""
	if err := s.persistLocked(before); err != nil {
		return RoutingOverride{}, err
	}
	return clone(s.state.Override), nil
}

// RoutingSplit returns the validated participants of a legacy manual override.
// Reads do not reserve requests, change concurrent pressure, or write state.
func (s *Service) RoutingSplit(provider string) (string, []RoutingShare) {
	s.mu.RLock()
	active := s.state.Override.State == "active" && s.state.Override.Mode == "split" && s.state.Override.Provider == provider
	s.mu.RUnlock()
	if !active {
		return "", nil
	}
	ids, temporary := s.overrideIdentities()
	s.mu.RLock()
	defer s.mu.RUnlock()
	o := s.state.Override
	if o.State != "active" || o.Mode != "split" || o.Provider != provider || s.overrideTerminationLocked(ids, temporary) != "" {
		return "", nil
	}
	return o.ID, clone(o.Accounts)
}

// Pressure overrides share the existing identity, expiry and cancellation journal.
// Percentages are intentionally rejected: concurrent attempts are not allowance.
func validPressure(accounts []RoutingShare) bool {
	return len(accounts) == 2 && strings.TrimSpace(accounts[0].CredentialID) != "" && strings.TrimSpace(accounts[1].CredentialID) != "" && accounts[0].CredentialID != accounts[1].CredentialID && accounts[0].Percent == 0 && accounts[1].Percent == 0
}

func (s *Service) ReplacePressureOverride(accounts []RoutingShare, hours int, revision uint64) (RoutingOverride, error) {
	if !validPressure(accounts) {
		return RoutingOverride{}, policyError("invalid_pressure", "exactly two distinct accounts without percentages are required")
	}
	return s.replaceRoutingOverride("pressure", accounts, hours, revision)
}

// RoutingPressure checks absolute expiry on every selection without maintenance
// polling, provider I/O, or a second writer to the policy journal.
func (s *Service) RoutingPressure(provider string) (string, []RoutingShare) {
	s.mu.RLock()
	active := s.state.Override.State == "active" && s.state.Override.Mode == "pressure" && s.state.Override.Provider == provider
	s.mu.RUnlock()
	if !active {
		return "", nil
	}
	ids, temporary := s.overrideIdentities()
	s.mu.RLock()
	defer s.mu.RUnlock()
	o := s.state.Override
	if o.State != "active" || o.Mode != "pressure" || o.Provider != provider || s.overrideTerminationLocked(ids, temporary) != "" {
		return "", nil
	}
	return o.ID, clone(o.Accounts)
}
