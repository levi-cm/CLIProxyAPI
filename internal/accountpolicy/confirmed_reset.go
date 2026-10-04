package accountpolicy

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ConfirmedResetRequest binds a short-lived human confirmation to one snapshot.
// RequestID is persisted in the native journal before the provider write.
type ConfirmedResetRequest struct {
	CredentialID     string    `json:"credential_id"`
	CreditID         string    `json:"credit_id"`
	RequestID        string    `json:"request_id"`
	ExpectedIdentity Identity  `json:"expected_identity"`
	ExpectedVersion  uint64    `json:"expected_version"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func (r ConfirmedResetRequest) Validate() error {
	key, err := uuid.Parse(r.RequestID)
	if err != nil || key == uuid.Nil || key.String() != r.RequestID || r.CredentialID == "" || r.CreditID == "" || r.ExpectedVersion == 0 || r.ExpiresAt.IsZero() || r.ExpectedIdentity.CredentialID != r.CredentialID || r.ExpectedIdentity.AccountID == "" || r.ExpectedIdentity.WorkspaceID == "" || r.ExpectedIdentity.Provider == "" {
		return policyError("invalid_request", "complete confirmation evidence and a canonical UUID are required")
	}
	return nil
}

func (s *Service) RedeemConfirmed(ctx context.Context, request ConfirmedResetRequest) (Operation, error) {
	if err := request.Validate(); err != nil {
		return Operation{}, err
	}
	return s.redeemSelected(ctx, request.CredentialID, request.CreditID, &request.ExpectedIdentity, &request)
}

func (s *Service) checkConfirmation(request *ConfirmedResetRequest, identity Identity) error {
	if request == nil {
		return nil
	}
	if !sameOwnership(request.ExpectedIdentity, identity) || request.ExpectedIdentity.Generation != identity.Generation {
		return policyError("conflict", "confirmed ownership changed")
	}
	if !request.ExpiresAt.After(s.now()) || request.ExpiresAt.After(s.now().Add(120*time.Second)) {
		return policyError("conflict", "confirmation expired or has an invalid lifetime")
	}
	return nil
}

// sameConfirmedEvidence ignores observation/version advancement, not semantic changes.
func sameConfirmedEvidence(before, after Snapshot, creditID string) bool {
	oldCredit, oldFound := findCredit(before, creditID)
	newCredit, newFound := findCredit(after, creditID)
	if !oldFound || !newFound || !sameCreditEvidence(oldCredit, newCredit) {
		return false
	}
	ordinary := func(snapshot Snapshot) []Bucket {
		var buckets []Bucket
		for _, bucket := range snapshot.Buckets {
			if bucket.Scope == "ordinary" {
				buckets = append(buckets, bucket)
			}
		}
		return buckets
	}
	oldBuckets, newBuckets := ordinary(before), ordinary(after)
	if len(oldBuckets) != len(newBuckets) {
		return false
	}
	for _, old := range oldBuckets {
		matches := 0
		for _, next := range newBuckets {
			if old.Scope != next.Scope || old.Model != next.Model || old.DurationSeconds != next.DurationSeconds {
				continue
			}
			matches++
			if old.UsedPercent != next.UsedPercent || !old.ResetAt.Equal(next.ResetAt) || old.Source != next.Source || (old.Allowed == nil) != (next.Allowed == nil) || old.Allowed != nil && *old.Allowed != *next.Allowed {
				return false
			}
		}
		if matches != 1 {
			return false
		}
	}
	return before.Eligible == after.Eligible && before.Status == after.Status
}
