package cliproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

type identityHistoryProvider struct {
	snapshot accountpolicy.Snapshot
	fail     bool
}

func (p *identityHistoryProvider) Discover(context.Context, accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	if p.fail {
		return accountpolicy.Snapshot{}, errors.New("fixture discovery failure")
	}
	return p.snapshot, nil
}
func (*identityHistoryProvider) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	return accountpolicy.ConsumeResult{}, nil
}

func TestHistoryIdentityBoundarySurvivesFailedDiscovery(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	identity := accountpolicy.Identity{CredentialID: "fixture", Provider: "codex", AccountID: "account-a"}
	p := &identityHistoryProvider{snapshot: accountpolicy.Snapshot{Identity: identity, ObservedAt: now, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, ObservedAt: now, UsedPercent: 50}}}}
	settings := accountpolicy.DefaultSettings()
	settings.StateDir = t.TempDir()
	settings.UsageHistoryEnabled = true
	collector := &policyObservationCollector{}
	s, err := accountpolicy.NewService(accountpolicy.Options{Settings: settings, Provider: p, Accounts: func() []accountpolicy.Identity { return []accountpolicy.Identity{identity} }, Now: func() time.Time { return now }, Observe: func(old, next accountpolicy.Snapshot, ops []accountpolicy.Operation) {
		archivePolicyObservation(collector, old, next, ops)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	identity.AccountID = "account-b"
	p.snapshot.Identity = identity
	p.fail = true
	if err = s.Refresh(context.Background(), "fixture"); err == nil {
		t.Fatal("expected discovery failure")
	}
	if retained, _ := s.Snapshot("fixture"); retained.Identity.AccountID != "account-a" {
		t.Fatal("old quota was relabelled as a different account")
	}
	p.fail = false
	now = now.Add(time.Minute)
	p.snapshot.ObservedAt = now
	p.snapshot.Buckets[0].ObservedAt = now
	if err = s.Refresh(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	if records := collector.records; len(records) != 2 || records[0].IdentityRef == records[1].IdentityRef || records[1].Event != "identity_change" {
		t.Fatalf("unrelated quota histories were joined: %+v", records)
	}
}
