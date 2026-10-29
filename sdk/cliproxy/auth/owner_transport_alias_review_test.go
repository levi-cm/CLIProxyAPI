package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

func TestOwnerTransportCooldownCanonicalizesResolvedModel(t *testing.T) {
	s, _, auths, now := durableBindingFixture(t)
	s.SetModelResolver(func(*Auth, string) string { return " upstream-model(high) " })
	a := auths[0]
	a.ModelStates = map[string]*ModelState{"upstream-model": {
		Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(time.Minute),
		LastError: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary upstream failure"},
	}}
	if got := statusCodeFromError(s.ownerTransportCooldown(a, "public-alias")); got != http.StatusServiceUnavailable {
		t.Fatalf("resolved-model cooldown status=%d, want 503", got)
	}
}

func TestOwnerTransportCooldownKeepsHardOwnerBlocks(t *testing.T) {
	for _, block := range []string{"model_disabled", "policy_hold", "policy_quota", "terminal_auth", "expired_token"} {
		t.Run(block, func(t *testing.T) {
			s, p, auths, now := durableBindingFixture(t)
			a := auths[0]
			a.ModelStates = map[string]*ModelState{"gpt-5": {
				Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(time.Minute),
				LastError: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary upstream failure"},
			}}
			switch block {
			case "model_disabled":
				a.ModelStates["gpt-5"].Status = StatusDisabled
			case "policy_hold":
				p.settings.Accounts = map[string]accountpolicy.AccountControl{a.ID: {Hold: true}}
			case "policy_quota":
				snapshot := p.snapshots[a.ID]
				snapshot.Buckets[0].UsedPercent = 100
				p.snapshots[a.ID] = snapshot
			case "terminal_auth":
				a.Unavailable, a.Status = true, StatusError
				a.LastError = &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"}
			case "expired_token":
				a.Metadata["access_token"] = "fixture-expired-access-token"
				a.Metadata["expired"] = now.Add(-time.Minute).Format(time.RFC3339)
			}
			if err := s.ownerTransportCooldown(a, "gpt-5"); err != nil {
				t.Fatalf("hard %s block was presented as temporary: %v", block, err)
			}
		})
	}
}
