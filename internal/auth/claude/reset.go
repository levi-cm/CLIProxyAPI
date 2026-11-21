package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Claude subscription accounts can hold limit-reset grants (the "cedar_ember"
// program behind Claude Code's reset offer). The usage endpoint lists them when
// asked, and reset_rate_limits spends one. Anthropic reports the program only to
// Claude Code clients, so these calls identify as the Claude CLI; the generic
// control-plane identity sees every account as ineligible ("surface").
const (
	// UsageWithResetsURL is UsageURL plus the reset-grant inventory.
	UsageWithResetsURL = UsageURL + "?cedar_ember=1&skip_spend=1"
	resetProgram       = "cedar_ember"
	resetClaimURL      = "https://api.anthropic.com/api/organizations/%s/reset_rate_limits"
	// resetClientUserAgent matches the CLI fingerprint the executor presents.
	resetClientUserAgent = "claude-cli/2.1.321 (external, cli)"
	resetClientBeta      = "oauth-2025-04-20"
	// resetHandshakeTimeout bounds connection setup only, not the response.
	resetHandshakeTimeout = 20 * time.Second
)

// Reset claim results reported by Anthropic. ResetResultRateLimited and
// ResetResultAuthError are local labels for refused submissions.
const (
	ResetResultReset       = "reset"
	ResetResultAlreadyUsed = "already_used"
	ResetResultNotLimited  = "not_limited"
	ResetResultCooldown    = "cooldown"
	ResetResultIneligible  = "ineligible"
	ResetResultUnavailable = "unavailable"
	ResetResultRateLimited = "rate_limited"
	ResetResultAuthError   = "auth_error"
)

var (
	resetOrganizationPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	resetGrantPattern        = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	resetRequestPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	resetResults             = []string{ResetResultReset, ResetResultAlreadyUsed, ResetResultNotLimited, ResetResultCooldown, ResetResultIneligible, ResetResultUnavailable}
)

// ErrResetOutcomeUnknown means a claim may or may not have spent the grant. The
// caller must retry with the same request ID or reconcile from the inventory.
var ErrResetOutcomeUnknown = errors.New("claude reset claim outcome is unknown")

// ResetGrant is one reset grant. Clears names the usage windows it empties,
// for example "seven_day" (weekly) or "seven_day_overage_included" (Fable).
type ResetGrant struct {
	ID                string             `json:"id"`
	Label             string             `json:"label"`
	ResetsTotal       int                `json:"resets_total"`
	ResetsLeft        int                `json:"resets_left"`
	StartsAt          *string            `json:"starts_at"`
	EndsAt            *string            `json:"ends_at"`
	Clears            []string           `json:"clears"`
	Paused            bool               `json:"paused"`
	UsableNow         bool               `json:"usable_now"`
	UseRequiresLimit  *bool              `json:"use_requires_limit"`
	PercentUsed       map[string]float64 `json:"percent_used"`
	Blocking          []json.RawMessage  `json:"blocking"`
	Type              string             `json:"type"`
	UnavailableReason *string            `json:"unavailable_reason"`
	GivesBackWeekly   bool               `json:"gives_back_weekly"`
}

// ResetProgram is the account's reset-grant state.
type ResetProgram struct {
	Eligible         bool         `json:"eligible"`
	IneligibleReason *string      `json:"ineligible_reason"`
	AtLimit          bool         `json:"at_limit"`
	Exhausted        []string     `json:"exhausted"`
	Grants           []ResetGrant `json:"grants"`
	WeeklyResetsAt   *string      `json:"weekly_resets_at"`
	CooldownUntil    *string      `json:"cooldown_until"`
}

// OAuthProfileForResets reads the account identity with the reset client's
// identity, so the claim and its ownership check come from one client.
func (o *ClaudeAuth) OAuthProfileForResets(ctx context.Context, accessToken string) (*OAuthProfile, error) {
	body, status, errDo := o.doResetRequest(ctx, http.MethodGet, ProfileURL, accessToken, nil)
	if errDo != nil {
		return nil, errDo
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch Claude OAuth profile failed with status %d", status)
	}
	var profile OAuthProfile
	if errDecode := json.Unmarshal(body, &profile); errDecode != nil {
		return nil, fmt.Errorf("parse Claude OAuth profile response: %w", errDecode)
	}
	return &profile, nil
}

// FetchOAuthUsageWithResets reads the plan utilization together with the
// reset-grant inventory. It never spends anything.
func (o *ClaudeAuth) FetchOAuthUsageWithResets(ctx context.Context, accessToken string) (*Usage, error) {
	body, status, errDo := o.doResetRequest(ctx, http.MethodGet, UsageWithResetsURL, accessToken, nil)
	if errDo != nil {
		return nil, errDo
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch Claude OAuth usage failed with status %d", status)
	}
	var usage Usage
	if errDecode := json.Unmarshal(body, &usage); errDecode != nil {
		return nil, fmt.Errorf("parse Claude OAuth usage response: %w", errDecode)
	}
	if usage.Resets == nil {
		return nil, fmt.Errorf("parse Claude OAuth usage response: reset inventory missing")
	}
	return &usage, nil
}

// ClaimRateLimitReset spends one reset from grantID. requestID makes the claim
// idempotent: a retry with the same ID reports already_used instead of spending
// a second reset. A nil error carries one of the ResetResult values; any other
// outcome returns ErrResetOutcomeUnknown.
func (o *ClaudeAuth) ClaimRateLimitReset(ctx context.Context, accessToken, organizationUUID, grantID, requestID string) (string, error) {
	if !resetOrganizationPattern.MatchString(organizationUUID) || !resetGrantPattern.MatchString(grantID) || !resetRequestPattern.MatchString(requestID) {
		return "", fmt.Errorf("claim Claude reset: invalid organization, grant or request id")
	}
	payload, errMarshal := json.Marshal(map[string]string{"program": resetProgram, "grant_id": grantID, "request_id": requestID})
	if errMarshal != nil {
		return "", fmt.Errorf("claim Claude reset: %w", errMarshal)
	}
	endpoint := fmt.Sprintf(resetClaimURL, strings.ToLower(organizationUUID))
	body, status, errDo := o.doResetRequest(ctx, http.MethodPost, endpoint, accessToken, payload)
	if errDo != nil {
		return "", fmt.Errorf("%w: %v", ErrResetOutcomeUnknown, errDo)
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ResetResultRateLimited, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ResetResultAuthError, nil
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		var decoded struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(body, &decoded) == nil {
			for _, known := range resetResults {
				if decoded.Result == known {
					return known, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%w: status %d", ErrResetOutcomeUnknown, status)
}

func (o *ClaudeAuth) doResetRequest(ctx context.Context, method, endpoint, accessToken string, payload []byte) ([]byte, int, error) {
	if o == nil || o.httpClient == nil {
		return nil, 0, fmt.Errorf("claude reset request: HTTP client is nil")
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, 0, fmt.Errorf("claude reset request: access token is empty")
	}
	var reader *bytes.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	// The transport detaches the dial from request cancellation; this value
	// survives that and bounds the custom TLS handshake.
	ctx = context.WithValue(ctx, claudeRefreshHandshakeTimeoutContextKey{}, resetHandshakeTimeout)
	var req *http.Request
	var errRequest error
	if reader != nil {
		req, errRequest = http.NewRequestWithContext(ctx, method, endpoint, reader)
	} else {
		req, errRequest = http.NewRequestWithContext(ctx, method, endpoint, nil)
	}
	if errRequest != nil {
		return nil, 0, fmt.Errorf("create Claude reset request: %w", errRequest)
	}
	req.Header.Set("User-Agent", resetClientUserAgent)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", resetClientBeta)
	// Each ClaudeAuth carries its own transport; never leave an idle connection.
	req.Header.Set("Connection", "close")
	req.Close = true
	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("claude reset request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close Claude reset response body: %v", errClose)
		}
	}()
	body, errRead := readClaudeOAuthResponseBody(resp)
	if errRead != nil {
		return nil, resp.StatusCode, fmt.Errorf("read Claude reset response: %w", errRead)
	}
	return body, resp.StatusCode, nil
}
