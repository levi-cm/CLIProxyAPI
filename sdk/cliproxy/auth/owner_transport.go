package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

// ownerTransportError preserves a bounded recovery or verified-owner cooldown
// decision. Its cause stays available to logs and errors.Is, while clients
// receive a content-free retryable failure.
type ownerTransportError struct {
	cause      error
	status     int
	retryAfter *time.Duration
}

func (e *ownerTransportError) Error() string {
	if e.status == http.StatusTooManyRequests {
		return `{"error":{"type":"rate_limit_error","code":"upstream_rate_limit","message":"The upstream rate limit was reached. Retry on the same conversation owner after the cooldown."}}`
	}
	return `{"error":{"type":"server_error","code":"upstream_transport_error","message":"The upstream connection failed. Retry on the same conversation owner."}}`
}
func (e *ownerTransportError) Unwrap() error              { return e.cause }
func (e *ownerTransportError) StatusCode() int            { return e.status }
func (e *ownerTransportError) RetryAfter() *time.Duration { return e.retryAfter }
func (e *ownerTransportError) SafeResponseHeaders() http.Header {
	if e.retryAfter == nil {
		return nil
	}
	return safeRetryAfterHeader(*e.retryAfter)
}

// IsTransportRecoveryError prevents outer handlers from replaying a turn after
// the manager has exhausted retries or determined that output makes replay unsafe.
func IsTransportRecoveryError(err error) bool {
	var transport *ownerTransportError
	return errors.As(err, &transport)
}
func publicTransportError(err error) error {
	if err == nil || IsTransportRecoveryError(err) || !isHTTP2TransportFailure(err) {
		return err
	}
	return &ownerTransportError{cause: err, status: http.StatusBadGateway}
}

// A verified owner's upstream cooldown is unavailability rather than an ownership
// conflict. Reporting its status never changes the owner or bypasses the cooldown.
func (s *EarliestDeadlineSelector) ownerTransportCooldown(auth *Auth, model string) error {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return nil
	}
	now := s.now()
	if expires, ok := auth.AccessTokenExpirationTime(); ok && !expires.IsZero() && !expires.After(now) {
		return nil
	}
	s.mu.Lock()
	resolve := s.modelResolver
	s.mu.Unlock()
	key := canonicalModelKey(model)
	if resolve != nil {
		key = canonicalModelKey(resolve(auth, model))
	}
	policyQuotaBlocked := false
	if s.policy != nil {
		if snapshot, exists := s.policy.Snapshot(auth.ID); exists {
			evaluation := accountpolicy.Evaluate(snapshot, key, s.policy.Settings(), now)
			if !evaluation.Eligible && evaluation.Reason != "quota_block" {
				return nil
			}
			policyQuotaBlocked = !evaluation.Eligible
		}
	}
	failure := auth.LastError
	until := auth.NextRetryAfter
	quota := auth.Quota
	state := auth.ModelStates[key]
	if state != nil && state.Status == StatusDisabled {
		return nil
	}
	// Auth-level non-credential quota and retry fields can be aggregates of
	// unrelated models. The requested model owns its own failure and deadline.
	if !quota.Exceeded || quota.Reason != "credential_quota" {
		if state != nil {
			failure, until, quota = state.LastError, state.NextRetryAfter, state.Quota
		} else if len(auth.ModelStates) > 0 {
			return nil
		}
	}
	if quota.Exceeded && quota.NextRecoverAt.After(until) {
		until = quota.NextRecoverAt
	}
	if failure == nil || !until.After(now) {
		return nil
	}
	status := http.StatusServiceUnavailable
	if failure.HTTPStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	} else if quota.Exceeded || policyQuotaBlocked || !isOwnerTemporaryFailure(failure) {
		return nil
	}
	delay := until.Sub(now)
	return &ownerTransportError{cause: failure, status: status, retryAfter: &delay}
}

func isOwnerTemporaryFailure(failure *Error) bool {
	if failure == nil {
		return false
	}
	if isTransientTransportResultError(failure) || isConnectionLifecycleResultError(failure) {
		return true
	}
	switch failure.HTTPStatus {
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// IsReplayForbiddenError exposes the manager's committed-generation boundary to
// outer bootstrap handlers, which must not start another manager request.
func IsReplayForbiddenError(err error) bool {
	return IsTransportRecoveryError(err) || isRequestInvalidError(err) || isRequestTerminatedError(err) || isCapacityError(err)
}
