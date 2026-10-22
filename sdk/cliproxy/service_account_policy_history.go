package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyledger"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func (s *Service) accountPolicyHistoryArchive() *accountpolicyledger.Archive {
	s.accountPolicyMu.Lock()
	a := s.accountPolicyHistory
	s.accountPolicyMu.Unlock()
	if a != nil && s.accountPolicy != nil && s.accountPolicy.Settings().UsageHistoryEnabled {
		s.accountPolicyHistorySeedMu.Lock()
		if !s.accountPolicyHistorySeeded {
			if archiveConfirmedOperations(a, s.accountPolicy.Operations()) == nil {
				s.accountPolicyHistorySeeded = true
			}
		}
		s.accountPolicyHistorySeedMu.Unlock()
	}
	return a
}

func (s *Service) startAccountPolicyHistory() {
	if s.accountPolicy == nil {
		return
	}
	settings := s.accountPolicy.Settings()
	a, err := accountpolicyledger.New(settings.StateDir, func() bool { return s.accountPolicy.Settings().UsageHistoryEnabled })
	if err != nil {
		log.WithField("component", "account-policy-history").Error("history archive unavailable; inference continues")
		return
	}
	s.accountPolicyMu.Lock()
	s.accountPolicyHistory = a
	s.accountPolicyMu.Unlock()
	if err = a.ImportLegacy(settings.StateDir); err != nil {
		log.WithField("component", "account-policy-history").Error("legacy history import incomplete; inference continues")
	}
	usage.RegisterNamedPlugin("account-policy-history", a)
	s.accountPolicy.SetObservationObserver(func(old, next accountpolicy.Snapshot, ops []accountpolicy.Operation) {
		_ = archiveConfirmedOperations(a, ops)
		archivePolicyObservation(a, old, next, ops)
	})
}
func (s *Service) closeAccountPolicyHistory(ctx context.Context) {
	a := s.accountPolicyHistoryArchive()
	if a == nil {
		return
	}
	usage.StopDefault()
	if err := usage.WaitDefault(ctx); err != nil {
		go func() { _ = usage.WaitDefault(context.Background()); _ = a.Close() }()
		return
	}
	if err := a.Close(); err != nil {
		log.WithField("component", "account-policy-history").Error("history close failed")
	}
}

type policyObservationSink interface {
	Observe(accountpolicyledger.Observation) error
}

func archivePolicyObservation(a policyObservationSink, old, next accountpolicy.Snapshot, ops []accountpolicy.Operation) {
	id := next.Identity
	h := sha256.Sum256([]byte(id.Provider + "\x00" + id.CredentialID + "\x00" + id.AccountID + "\x00" + id.WorkspaceID))
	ref := hex.EncodeToString(h[:])
	same := old.Identity.CredentialID == id.CredentialID && old.Identity.Provider == id.Provider && old.Identity.AccountID == id.AccountID && old.Identity.WorkspaceID == id.WorkspaceID
	for i, b := range next.Buckets {
		event := "observation"
		if !same && old.Identity.CredentialID != "" {
			event = "identity_change"
		}
		opID := ""
		if same {
			for _, prev := range old.Buckets {
				if prev.Scope != b.Scope || prev.Model != b.Model || prev.DurationSeconds != b.DurationSeconds || b.UsedPercent >= prev.UsedPercent {
					continue
				}
				event = "allowance_change"
				if !prev.ResetAt.IsZero() && !b.ObservedAt.Before(prev.ResetAt) && b.ResetAt.After(prev.ResetAt) {
					event = "natural_refill"
				}
			}
		}
		for _, op := range ops {
			if op.State == "confirmed" && op.After != nil && op.CredentialID == id.CredentialID && op.After.Identity.AccountID == id.AccountID && op.After.Identity.WorkspaceID == id.WorkspaceID && op.After.Version == next.Version {
				event = "manual_reset"
				opID = op.ID
				break
			}
		}
		recordID := fmt.Sprintf("%s:%d:%d", ref, next.Version, i)
		if event == "manual_reset" {
			recordID = fmt.Sprintf("%s:manual:%s:%d", ref, opID, i)
		}
		_ = a.Observe(accountpolicyledger.Observation{ID: recordID, CredentialID: id.CredentialID, IdentityRef: ref, Provider: id.Provider, Scope: b.Scope, Model: b.Model, DurationSeconds: b.DurationSeconds, ObservedAt: b.ObservedAt, UsedPercent: b.UsedPercent, RemainingPercent: 100 - b.UsedPercent, Allowed: b.Allowed, ResetAt: b.ResetAt, Event: event, OperationID: opID})
	}
}

type policyObservationCollector struct {
	records []accountpolicyledger.Observation
}

func (c *policyObservationCollector) Observe(o accountpolicyledger.Observation) error {
	c.records = append(c.records, o)
	return nil
}
func archiveConfirmedOperations(a *accountpolicyledger.Archive, ops []accountpolicy.Operation) error {
	collector := &policyObservationCollector{}
	for _, op := range ops {
		if op.State != "confirmed" || op.After == nil {
			continue
		}
		before := accountpolicy.Snapshot{}
		if op.Before != nil {
			before = *op.Before
		}
		archivePolicyObservation(collector, before, *op.After, []accountpolicy.Operation{op})
	}
	for _, o := range collector.records {
		if err := a.Observe(o); err != nil {
			return err
		}
	}
	return nil
}
