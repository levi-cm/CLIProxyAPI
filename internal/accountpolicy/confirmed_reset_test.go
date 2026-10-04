package accountpolicy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func confirmedFixture(t *testing.T) (*Service, Options, *fakeProvider, *time.Time, ConfirmedResetRequest) {
	t.Helper()
	now := testTime()
	a := testSnapshot("a", 7)
	expiry := now.Add(time.Hour)
	a.Credits = []Credit{testCredit("first", &expiry), testCredit("second", nil)}
	a.AvailableCredits = 2
	a.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"a": a}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, opts := fixtureService(t, p, &now, settings)
	snapshot, _ := s.Snapshot("a")
	request := ConfirmedResetRequest{CredentialID: "a", CreditID: "first", RequestID: "98d26941-fcc9-4b03-bcc4-789fb28c86d1", ExpectedIdentity: snapshot.Identity, ExpectedVersion: snapshot.Version, ExpiresAt: now.Add(120 * time.Second)}
	return s, opts, p, &now, request
}

func TestConfirmedResetRejectsInvalidatedConfirmationWithoutProviderWrite(t *testing.T) {
	for _, kind := range []string{"version", "account", "workspace", "generation", "provider", "expired", "too_long", "bad_uuid", "other_credential"} {
		t.Run(kind, func(t *testing.T) {
			s, _, p, now, request := confirmedFixture(t)
			switch kind {
			case "version":
				request.ExpectedVersion++
			case "account":
				request.ExpectedIdentity.AccountID = "other"
			case "workspace":
				request.ExpectedIdentity.WorkspaceID = "other"
			case "generation":
				request.ExpectedIdentity.Generation++
			case "provider":
				request.ExpectedIdentity.Provider = "other"
			case "expired":
				request.ExpiresAt = *now
			case "too_long":
				request.ExpiresAt = now.Add(121 * time.Second)
			case "bad_uuid":
				request.RequestID = "not-a-uuid"
			case "other_credential":
				request.ExpectedIdentity.CredentialID = "b"
			}
			if _, err := s.RedeemConfirmed(context.Background(), request); err == nil {
				t.Fatal("invalid confirmation accepted")
			}
			if len(p.requests) != 0 || len(s.Operations()) != 0 {
				t.Fatal("invalid confirmation wrote or journaled a reset")
			}
		})
	}
}

func TestConfirmedResetDuplicateAndRestartReturnOneJournaledRequest(t *testing.T) {
	s, opts, p, now, request := confirmedFixture(t)
	p.consume = func(id Identity, key, credit string) (ConsumeResult, error) {
		if key != request.RequestID || credit != "first" {
			t.Fatal("caller operation identity changed")
		}
		a := p.snapshots[id.CredentialID]
		a.Credits = a.Credits[1:]
		a.AvailableCredits = 1
		a.Buckets[1].UsedPercent = 5
		a.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
		p.snapshots[id.CredentialID] = a
		return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
	}
	first, err := s.RedeemConfirmed(context.Background(), request)
	if err != nil || first.State != "confirmed" || first.RequestID != request.RequestID {
		t.Fatalf("first result: %+v %v", first, err)
	}
	*now = now.Add(10 * time.Minute)
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := restarted.RedeemConfirmed(context.Background(), request)
	if err != nil || duplicate.ID != first.ID || len(p.requests) != 1 {
		t.Fatalf("duplicate replay: %+v %v writes=%d", duplicate, err, len(p.requests))
	}
	request.CreditID = "second"
	if _, err = restarted.RedeemConfirmed(context.Background(), request); err == nil {
		t.Fatal("idempotency key retargeted another credit")
	}
	if len(p.requests) != 1 {
		t.Fatal("retargeted provider write")
	}
}

func TestConfirmedResetUnknownReplayDoesNotRetryProvider(t *testing.T) {
	s, opts, p, _, request := confirmedFixture(t)
	p.consume = func(Identity, string, string) (ConsumeResult, error) {
		return ConsumeResult{}, errors.New("lost response")
	}
	first, err := s.RedeemConfirmed(context.Background(), request)
	if err == nil || first.State != "outcome_unknown" {
		t.Fatal("unknown write not journaled")
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := restarted.RedeemConfirmed(context.Background(), request)
	if err != nil || duplicate.ID != first.ID || duplicate.State != "outcome_unknown" || len(p.requests) != 1 {
		t.Fatalf("unknown replay retried provider: %+v %v", duplicate, err)
	}
}

func TestConfirmedResetConcurrentCallbacksWriteExactlyOnce(t *testing.T) {
	s, _, p, _, request := confirmedFixture(t)
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.RedeemConfirmed(context.Background(), request) }()
	}
	wg.Wait()
	if len(p.requests) != 1 || len(s.Operations()) != 1 {
		t.Fatal("concurrent confirmation duplicated provider call")
	}
}

func TestConfirmedResetExpiresDuringFreshDiscoveryBeforeSubmission(t *testing.T) {
	s, _, p, now, request := confirmedFixture(t)
	p.discover = func(id Identity) (Snapshot, error) {
		*now = now.Add(121 * time.Second)
		return p.snapshots[id.CredentialID], nil
	}
	if _, err := s.RedeemConfirmed(context.Background(), request); err == nil {
		t.Fatal("confirmation expired during discovery was submitted")
	}
	if len(p.requests) != 0 {
		t.Fatal("expired confirmation consumed provider credit")
	}
}

func TestConfirmedResetRejectsChangedEvidenceDuringDiscovery(t *testing.T) {
	for _, kind := range []string{"allowance", "weekly_refresh", "credit_expiry", "credit_scopes"} {
		t.Run(kind, func(t *testing.T) {
			s, _, p, now, request := confirmedFixture(t)
			p.discover = func(id Identity) (Snapshot, error) {
				a := p.snapshots[id.CredentialID]
				switch kind {
				case "allowance":
					a.Buckets[1].UsedPercent = 90
				case "weekly_refresh":
					a.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
				case "credit_expiry":
					expiry := now.Add(2 * time.Hour)
					a.Credits[0].ExpiresAt = &expiry
				case "credit_scopes":
					a.Credits[0].Scopes = []string{"ordinary", "other"}
				}
				return a, nil
			}
			if _, err := s.RedeemConfirmed(context.Background(), request); err == nil {
				t.Fatal("changed confirmation evidence consumed")
			}
			if len(p.requests) != 0 || len(s.Operations()) != 0 {
				t.Fatal("changed evidence wrote or journaled")
			}
		})
	}
}
