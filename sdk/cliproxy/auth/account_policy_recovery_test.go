package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

func recoveryFixture() (*Manager, accountpolicy.Snapshot, accountpolicy.Snapshot) {
	now := time.Now().UTC()
	allowed := true
	blocked := false
	identity := accountpolicy.Identity{CredentialID: "a", AccountID: "account-a", WorkspaceID: "workspace-a", Provider: "codex", Generation: 7}
	before := accountpolicy.Snapshot{Identity: identity, Version: 1, ObservedAt: now, Eligible: false, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", Model: "gpt-5", DurationSeconds: 604800, UsedPercent: 100, Allowed: &blocked, ObservedAt: now, ResetAt: now.Add(24 * time.Hour)}}}
	after := accountpolicy.Snapshot{Identity: identity, Version: 2, ObservedAt: now.Add(time.Second), Eligible: true, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", Model: "gpt-5", DurationSeconds: 604800, UsedPercent: 15, Allowed: &allowed, ObservedAt: now.Add(time.Second), ResetAt: now.Add(7 * 24 * time.Hour)}}}
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex", Generation: 7, Status: StatusError, UpdatedAt: now.Add(-time.Second), LastError: &Error{HTTPStatus: 429}, ModelStates: map[string]*ModelState{
		"gpt-5":     {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)}, LastError: &Error{HTTPStatus: 429}, UpdatedAt: now.Add(-time.Second)},
		"unrelated": {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)}, LastError: &Error{HTTPStatus: 429}, UpdatedAt: now.Add(-time.Second)},
	}}
	return m, before, after
}

// Recovery must clear the covered model, preserve another quota, and persist it.
func TestPolicyRecoveryOnlyClearsCoveredQuotaAndPersists(t *testing.T) {
	m, before, after := recoveryFixture()
	store := &recordingCooldownStateStore{}
	m.SetCooldownStateStore(store)
	if err := m.RecoverPolicyQuota(context.Background(), before, after); err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetByID("a")
	if a.ModelStates["gpt-5"].Quota.Exceeded || a.ModelStates["gpt-5"].Unavailable {
		t.Fatal("covered quota remains blocked")
	}
	if !a.ModelStates["unrelated"].Quota.Exceeded || a.ModelStates["unrelated"].LastError == nil {
		t.Fatal("unrelated model failure cleared")
	}
	records := store.savedRecords()
	if len(records) != 1 || records[0].Model != "unrelated" {
		t.Fatalf("persisted records=%+v", records)
	}
}

func TestPolicyRecoveryPreservesDisabledAuthenticationAndNewFailures(t *testing.T) {
	for _, tc := range []string{"disabled", "authentication", "generation", "failure_timestamp", "ownership", "still_blocked", "unknown_allowance", "older_snapshot", "unknown_scope"} {
		t.Run(tc, func(t *testing.T) {
			m, before, after := recoveryFixture()
			a := m.auths["a"]
			switch tc {
			case "disabled":
				a.Disabled = true
			case "authentication":
				a.LastError = &Error{HTTPStatus: 401}
				a.StatusMessage = "unauthorized"
			case "generation":
				a.Generation++
			case "failure_timestamp":
				a.UpdatedAt = before.ObservedAt.Add(time.Nanosecond)
			case "ownership":
				after.Identity.AccountID = "different"
			case "still_blocked":
				allowed := false
				after.Buckets[0].Allowed = &allowed
			case "unknown_allowance":
				after.Buckets[0].Allowed = nil
			case "older_snapshot":
				after.ObservedAt = before.ObservedAt.Add(-time.Second)
			case "unknown_scope":
				before.Buckets[0].Scope = "unknown"
				after.Buckets[0].Scope = "unknown"
			}
			_ = m.RecoverPolicyQuota(context.Background(), before, after)
			got, _ := m.GetByID("a")
			if !got.ModelStates["gpt-5"].Quota.Exceeded {
				t.Fatal("protected failure cleared")
			}
		})
	}
}
