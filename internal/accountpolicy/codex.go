package accountpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CodexProtocolRevision pins the sanitized fixtures and backend protocol reference.
const CodexProtocolRevision = "afb436df8b70bb5bc57b86d9a3e829968988cd21"

// CodexCredential is supplied by the existing credential manager. AccountID is
// the account/workspace selected by ChatGPT-Account-Id, not the OAuth user ID.
type CodexCredential struct{ AccessToken, AccountID, WorkspaceID string }

// CodexClient never derives a backend URL from upstream account metadata.
// BaseURL is trusted deployment configuration; plain HTTP is fixture-only localhost.
type CodexClient struct {
	Credential              func(context.Context, string) (CodexCredential, error)
	RefreshCredential       func(context.Context, string) error
	HTTPClient              *http.Client
	HTTPClientForCredential func(string) *http.Client
	BaseURL                 string
	PathStyle               string
	transports              sync.Map
}

// CodexError exposes safe error categories and rate-limit advice without bodies,
// credentials, transport URLs, or error strings returned by credential callbacks.
type CodexError struct {
	Code       string
	StatusCode int
	RetryAfter time.Duration
}

func (e *CodexError) Error() string             { return "codex account policy: " + e.Code }
func (e *CodexError) RetryDelay() time.Duration { return e.RetryAfter }
func codexError(code string) error              { return &CodexError{Code: code} }

var _ Provider = (*CodexClient)(nil)

type codexWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	Duration    *int64   `json:"limit_window_seconds"`
	ResetAt     *int64   `json:"reset_at"`
}
type codexLimit struct {
	Allowed      *bool        `json:"allowed"`
	LimitReached *bool        `json:"limit_reached"`
	Primary      *codexWindow `json:"primary_window"`
	Secondary    *codexWindow `json:"secondary_window"`
}
type codexUsage struct {
	AccountID  string      `json:"account_id"`
	Plan       string      `json:"plan_type"`
	RateLimit  *codexLimit `json:"rate_limit"`
	Additional []struct {
		Scope string      `json:"metered_feature"`
		Name  string      `json:"limit_name"`
		Model string      `json:"normal_model_slug"`
		Limit *codexLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	CodeReview       *codexLimit `json:"code_review_rate_limit"`
	CodeReviewPlural *codexLimit `json:"code_review_rate_limits"`
	Inventory        *struct {
		Count *int `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
	SpendControl *struct {
		Reached bool `json:"reached"`
	} `json:"spend_control"`
}
type codexInventory struct {
	Count   *int `json:"available_count"`
	Credits *[]struct {
		ID      string          `json:"id"`
		Type    string          `json:"reset_type"`
		Status  string          `json:"status"`
		Granted string          `json:"granted_at"`
		Expires json.RawMessage `json:"expires_at"`
		Title   string          `json:"title"`
	} `json:"credits"`
}

func (c *CodexClient) credentials(ctx context.Context, id Identity) (CodexCredential, error) {
	if c.Credential == nil || id.Provider != "codex" || id.CredentialID == "" || id.AccountID == "" {
		return CodexCredential{}, codexError("identity_unavailable")
	}
	credential, err := c.Credential(ctx, id.CredentialID)
	if err != nil {
		return CodexCredential{}, codexError("credential_unavailable")
	}
	if credential.AccessToken == "" || credential.AccountID != id.AccountID {
		return CodexCredential{}, codexError("account_identity_mismatch")
	}
	// This pinned backend routes on account/workspace ID only. Distinct workspace
	// IDs require a verified provider-specific identity contract before writes.
	if id.WorkspaceID != "" && id.WorkspaceID != id.AccountID {
		return CodexCredential{}, codexError("workspace_identity_unsupported")
	}
	if credential.WorkspaceID != "" && credential.WorkspaceID != id.AccountID {
		return CodexCredential{}, codexError("workspace_identity_mismatch")
	}
	if strings.ContainsAny(credential.AccessToken+credential.AccountID, "\r\n") {
		return CodexCredential{}, codexError("credential_invalid")
	}
	return credential, nil
}

func (c *CodexClient) endpoint(resource string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://chatgpt.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", codexError("backend_configuration_invalid")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", codexError("backend_configuration_invalid")
	}
	style := c.PathStyle
	if style == "" {
		style = "chatgpt"
	}
	switch style {
	case "chatgpt":
		if u.Path != "" && u.Path != "/backend-api" {
			return "", codexError("backend_configuration_invalid")
		}
		u.Path = "/backend-api/wham/" + resource
	case "codex":
		if u.Path != "" {
			return "", codexError("backend_configuration_invalid")
		}
		u.Path = "/api/codex/" + resource
	default:
		return "", codexError("backend_path_style_invalid")
	}
	return u.String(), nil
}

func (c *CodexClient) request(ctx context.Context, id Identity, method, resource string, payload []byte) ([]byte, error) {
	endpoint, err := c.endpoint(resource)
	if err != nil {
		return nil, err
	}
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	if c.HTTPClientForCredential != nil {
		if selected := c.HTTPClientForCredential(id.CredentialID); selected != nil {
			client = *selected
		}
	}
	client.Timeout = 0
	// Never follow even a same-host redirect: all credential routing is explicit.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if original, ok := transport.(*http.Transport); ok && (original.ResponseHeaderTimeout != 0 || original.TLSHandshakeTimeout != 0) {
		cached, found := c.transports.Load(original)
		if !found {
			clone := original.Clone()
			clone.ResponseHeaderTimeout = 0
			clone.TLSHandshakeTimeout = 0
			cached, _ = c.transports.LoadOrStore(original, clone)
		}
		client.Transport = cached.(*http.Transport)
	}
	for attempt := 0; attempt < 2; attempt++ {
		credential, errCredential := c.credentials(ctx, id)
		if errCredential != nil {
			return nil, errCredential
		}
		req, errRequest := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if errRequest != nil {
			return nil, codexError("request_invalid")
		}
		req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		req.Header.Set("ChatGPT-Account-Id", credential.AccountID)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "CLIProxyAPI-account-policy")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, errResponse := client.Do(req)
		if errResponse != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, codexError("network_failure")
		}
		body, errRead := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
		errClose := response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 && c.RefreshCredential != nil {
			if errRefresh := c.RefreshCredential(ctx, id.CredentialID); errRefresh != nil {
				return nil, codexError("credential_refresh_failed")
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			code := "http_failure"
			if response.StatusCode == 401 || response.StatusCode == 403 {
				code = "authentication_rejected"
			}
			if response.StatusCode == 429 {
				code = "rate_limited"
			}
			if response.StatusCode >= 300 && response.StatusCode < 400 {
				code = "redirect_rejected"
			}
			retryAfter := time.Duration(0)
			if seconds, errSeconds := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64); errSeconds == nil && seconds > 0 && seconds < 86400 {
				retryAfter = time.Duration(seconds) * time.Second
			} else if at, errDate := http.ParseTime(response.Header.Get("Retry-After")); errDate == nil {
				retryAfter = time.Until(at)
				if retryAfter < 0 {
					retryAfter = 0
				}
			}
			return nil, &CodexError{Code: code, StatusCode: response.StatusCode, RetryAfter: retryAfter}
		}
		if errRead != nil || errClose != nil {
			return nil, codexError("response_read_failed")
		}
		if len(body) > 2*1024*1024 {
			return nil, codexError("response_too_large")
		}
		return body, nil
	}
	return nil, codexError("authentication_rejected")
}

func (c *CodexClient) Discover(ctx context.Context, id Identity) (snapshot Snapshot, err error) {
	snapshot = Snapshot{Identity: id, Status: "unknown", WritesDisabled: true}
	defer func() {
		var upstream *CodexError
		if errors.As(err, &upstream) && (strings.Contains(upstream.Code, "schema") || strings.Contains(upstream.Code, "identity")) {
			snapshot.WritesDisabled = true
			snapshot.LastError = upstream.Code
		}
	}()
	body, err := c.request(ctx, id, http.MethodGet, "usage", nil)
	if err != nil {
		return snapshot, err
	}
	var usage codexUsage
	if json.Unmarshal(body, &usage) != nil || usage.Plan == "" {
		return snapshot, codexError("usage_schema_changed")
	}
	if usage.AccountID != "" && usage.AccountID != id.AccountID {
		return snapshot, codexError("account_identity_mismatch")
	}
	now := time.Now().UTC()
	snapshot.Plan = usage.Plan
	snapshot.ObservedAt = now
	snapshot.Status = "ready"
	snapshot.Eligible = true
	snapshot.WritesDisabled = usage.AccountID == ""
	if snapshot.WritesDisabled {
		snapshot.LastError = "account_identity_unverified"
	}
	addLimit := func(scope, model string, limit *codexLimit) error {
		if limit == nil {
			return nil
		}
		if limit.Allowed == nil || limit.LimitReached == nil {
			snapshot.WritesDisabled = true
			snapshot.LastError = "usage_schema_changed"
		}
		if scope == "ordinary" && ((limit.Allowed != nil && !*limit.Allowed) || (limit.LimitReached != nil && *limit.LimitReached)) {
			snapshot.Eligible = false
			snapshot.Status = "quota_blocked"
		}
		for _, window := range []*codexWindow{limit.Primary, limit.Secondary} {
			if window == nil {
				continue
			}
			if window.UsedPercent == nil || window.Duration == nil || window.ResetAt == nil || *window.Duration <= 0 || *window.ResetAt <= 0 || *window.UsedPercent < 0 || *window.UsedPercent > 100 {
				return codexError("usage_schema_changed")
			}
			snapshot.Buckets = append(snapshot.Buckets, Bucket{Scope: scope, Model: model, DurationSeconds: *window.Duration, UsedPercent: *window.UsedPercent, Allowed: limit.Allowed, ResetAt: time.Unix(*window.ResetAt, 0).UTC(), ObservedAt: now, Source: "codex_usage"})
		}
		return nil
	}
	if errLimit := addLimit("ordinary", "", usage.RateLimit); errLimit != nil {
		return snapshot, errLimit
	}
	ordinaryKnown := len(snapshot.Buckets) > 0
	for _, additional := range usage.Additional {
		if additional.Scope == "" || additional.Name == "" {
			return snapshot, codexError("usage_schema_changed")
		}
		if errLimit := addLimit(additional.Scope, additional.Model, additional.Limit); errLimit != nil {
			return snapshot, errLimit
		}
	}
	for _, review := range []*codexLimit{usage.CodeReview, usage.CodeReviewPlural} {
		if review != nil {
			if errLimit := addLimit("code_review", "", review); errLimit != nil {
				return snapshot, errLimit
			}
			break
		}
	}
	if !ordinaryKnown {
		snapshot.WritesDisabled = true
		snapshot.LastError = "ordinary_allowance_unknown"
	}
	if usage.SpendControl != nil && usage.SpendControl.Reached {
		snapshot.Eligible = false
		snapshot.Status = "spend_control_blocked"
	}
	if usage.Inventory != nil {
		if usage.Inventory.Count == nil || *usage.Inventory.Count < 0 {
			return snapshot, codexError("inventory_schema_changed")
		}
		snapshot.AvailableCredits = *usage.Inventory.Count
	}
	body, err = c.request(ctx, id, http.MethodGet, "rate-limit-reset-credits", nil)
	if err != nil {
		var upstream *CodexError
		if errors.As(err, &upstream) && (upstream.StatusCode == 404 || upstream.StatusCode == 405) {
			snapshot.LastError = "credit_inventory_unsupported"
			snapshot.WritesDisabled = true
			return snapshot, nil
		}
		return snapshot, err
	}
	var inventory codexInventory
	if json.Unmarshal(body, &inventory) != nil || inventory.Count == nil || inventory.Credits == nil || *inventory.Count < 0 {
		return snapshot, codexError("inventory_schema_changed")
	}
	snapshot.InventoryObservedAt = time.Now().UTC()
	snapshot.AvailableCredits = *inventory.Count
	availableDetails := 0
	seen := map[string]bool{}
	for _, item := range *inventory.Credits {
		if item.ID == "" || item.Type == "" || item.Status == "" || item.Granted == "" || seen[item.ID] {
			return snapshot, codexError("inventory_schema_changed")
		}
		seen[item.ID] = true
		granted, errGranted := time.Parse(time.RFC3339, item.Granted)
		if errGranted != nil {
			return snapshot, codexError("inventory_schema_changed")
		}
		credit := Credit{ID: item.ID, Type: item.Type, Status: item.Status, Title: item.Title, GrantedAt: granted.UTC(), DetailsKnown: len(item.Expires) > 0}
		if len(item.Expires) > 0 && string(item.Expires) != "null" {
			var expiry string
			if json.Unmarshal(item.Expires, &expiry) != nil {
				return snapshot, codexError("inventory_schema_changed")
			}
			at, errExpiry := time.Parse(time.RFC3339, expiry)
			if errExpiry != nil {
				return snapshot, codexError("inventory_schema_changed")
			}
			at = at.UTC()
			credit.ExpiresAt = &at
		}
		if item.Type == "codex_rate_limits" {
			credit.Scopes = []string{"ordinary"}
		}
		if item.Status == "available" && credit.DetailsKnown {
			availableDetails++
		}
		snapshot.Credits = append(snapshot.Credits, credit)
	}
	snapshot.InventoryComplete = availableDetails == snapshot.AvailableCredits
	return snapshot, nil
}

func (c *CodexClient) Consume(ctx context.Context, id Identity, requestID, creditID string) (ConsumeResult, error) {
	if _, err := uuid.Parse(requestID); err != nil || creditID == "" {
		return ConsumeResult{}, codexError("redemption_selection_invalid")
	}
	// The journal owns retries. Revalidate ownership and the selected credit on
	// every attempt; the stable caller-supplied UUID is never regenerated here.
	snapshot, err := c.Discover(ctx, id)
	if err != nil {
		return ConsumeResult{}, err
	}
	if snapshot.WritesDisabled {
		return ConsumeResult{}, codexError("automatic_writes_disabled")
	}
	for _, credit := range snapshot.Credits {
		if credit.ID == creditID {
			if credit.Type != "codex_rate_limits" || (credit.Status != "available" && credit.Status != "redeemed") || !credit.DetailsKnown {
				return ConsumeResult{}, codexError("selected_credit_unavailable")
			}
			break
		}
	}
	// An absent or redeemed credit can be the result of a previously submitted
	// operation whose response was lost. Only the provider can reconcile its UUID.
	payload, errMarshal := json.Marshal(struct {
		RequestID string `json:"redeem_request_id"`
		CreditID  string `json:"credit_id"`
	}{requestID, creditID})
	if errMarshal != nil {
		return ConsumeResult{}, codexError("request_invalid")
	}
	body, errRequest := c.request(ctx, id, http.MethodPost, "rate-limit-reset-credits/consume", payload)
	if errRequest != nil {
		return ConsumeResult{}, errRequest
	}
	var result struct {
		Code    string `json:"code"`
		Windows *int   `json:"windows_reset"`
	}
	if json.Unmarshal(body, &result) != nil {
		return ConsumeResult{}, codexError("consume_schema_changed")
	}
	switch result.Code {
	case "reset", "nothing_to_reset", "no_credit", "already_redeemed":
	default:
		return ConsumeResult{}, codexError("consume_schema_changed")
	}
	windows := 0
	if result.Windows != nil {
		windows = *result.Windows
	}
	if windows < 0 {
		return ConsumeResult{}, codexError("consume_schema_changed")
	}
	return ConsumeResult{Code: result.Code, WindowsReset: windows}, nil
}
