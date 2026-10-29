package auth

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOwnerTransportCooldownRequestedModelEqualAggregatedDeadline(t *testing.T) {
	s, _, auths, now := durableBindingFixture(t)
	a := auths[0]
	temporary := &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary upstream failure"}
	hardSibling := &Error{HTTPStatus: http.StatusNotFound, Message: "sibling model not found"}
	a.ModelStates = map[string]*ModelState{
		"gpt-5":   {Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(5 * time.Second), LastError: temporary},
		"sibling": {Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(12 * time.Hour), LastError: hardSibling},
	}
	// The latest model failure is credential metadata, while the aggregate
	// deadline remains the earliest blocked model's deadline.
	a.LastError = hardSibling
	updateAggregatedAvailability(a, now)
	if !a.NextRetryAfter.Equal(a.ModelStates["gpt-5"].NextRetryAfter) {
		t.Fatal("fixture did not produce equal aggregate and requested-model deadlines")
	}
	err := s.ownerTransportCooldown(a, "gpt-5")
	if got := statusCodeFromError(err); got != http.StatusServiceUnavailable {
		t.Fatalf("temporary requested-model cooldown returned status %d, want 503: %v", got, err)
	}
	if got := SafeResponseHeaders(err).Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After=%q, want 5", got)
	}
}

func TestOwnerTransportCooldownRequestedHardModelEqualAggregatedDeadline(t *testing.T) {
	s, _, auths, now := durableBindingFixture(t)
	a := auths[0]
	until := now.Add(time.Hour)
	a.ModelStates = map[string]*ModelState{
		"gpt-5": {Unavailable: true, Status: StatusError, NextRetryAfter: until,
			LastError: &Error{HTTPStatus: http.StatusNotFound, Message: "model not found"}},
		"sibling": {Unavailable: true, Status: StatusError, NextRetryAfter: until,
			LastError: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary upstream failure"}},
	}
	a.LastError = a.ModelStates["sibling"].LastError
	updateAggregatedAvailability(a, now)
	if err := s.ownerTransportCooldown(a, "gpt-5"); err != nil {
		t.Fatalf("sibling temporary failure masked requested-model hard failure: %v", err)
	}
}

func TestVerifiedOwnerQuotaCooldownKeepsOwnerAndRetryAfter(t *testing.T) {
	for _, credentialQuota := range []bool{false, true} {
		t.Run(map[bool]string{false: "model", true: "credential"}[credentialQuota], func(t *testing.T) {
			s, p, auths, now := durableBindingFixture(t)
			owner := successfulBinding(t, s, auths, bindingOptions("quota-owned", "caller", `{"input":"hello"}`))
			addExpiringCredit(p, now)
			a := auths[0]
			if a.ID != owner {
				t.Fatalf("fixture owner=%s, want %s", owner, a.ID)
			}
			until := now.Add(37 * time.Second)
			failure := &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Rate limit exceeded"}
			a.ModelStates = map[string]*ModelState{"gpt-5": {
				Unavailable: true, Status: StatusError, NextRetryAfter: until, LastError: failure,
				Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until},
			}}
			a.LastError = failure
			updateAggregatedAvailability(a, now)
			if credentialQuota {
				a.Quota.Reason = "credential_quota"
			}
			before := a.Clone()
			opts := bindingOptions("quota-owned", "caller", `{"input":[{"type":"reasoning","encrypted_content":"fixture-opaque"}]}`)
			for attempt := 0; attempt < 2; attempt++ {
				picked, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
				if picked != nil || statusCodeFromError(err) != http.StatusTooManyRequests {
					t.Fatalf("attempt=%d owner=%v error=%v, want blocked owner with 429", attempt, picked, err)
				}
				if got := SafeResponseHeaders(err).Get("Retry-After"); got != "37" {
					t.Fatalf("Retry-After=%q, want 37", got)
				}
				if body := err.Error(); !strings.Contains(body, `"type":"rate_limit_error"`) || !strings.Contains(body, `"code":"upstream_rate_limit"`) || strings.Contains(body, failure.Message) {
					t.Fatalf("quota response must be a content-free rate-limit error: %s", body)
				}
				if !reflect.DeepEqual(a, before) {
					t.Fatal("owner quota or cooldown state was changed")
				}
			}
			changed := p.snapshots[owner]
			changed.Identity.AccountID = "different-account"
			p.snapshots[owner] = changed
			if picked, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths); picked != nil || statusCodeFromError(err) != http.StatusConflict {
				t.Fatalf("identity mismatch owner=%v error=%v, want fail-closed 409", picked, err)
			}
		})
	}
}
