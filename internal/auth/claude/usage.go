package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UsageURL is the read-only OAuth endpoint behind Claude Code's /usage view. It
// reports plan window utilization without sending a model request, so it costs
// no allowance.
const UsageURL = "https://api.anthropic.com/api/oauth/usage"

// UsageWindow is one plan window. Utilization is a percentage (0-100).
type UsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// UsageLimit is one entry of the usage response's limit list. It also carries
// windows without a dedicated field, such as the Fable-scoped weekly window.
type UsageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// Usage holds the plan windows the router understands. A nil window means the
// account currently has no such window open.
type Usage struct {
	FiveHour *UsageWindow `json:"five_hour"`
	SevenDay *UsageWindow `json:"seven_day"`
	Limits   []UsageLimit `json:"limits"`
}

// limitWindow returns the first listed limit of kind, optionally scoped to the
// model with the given display name, as a plan window.
func (u *Usage) limitWindow(kind, model string) *UsageWindow {
	for _, limit := range u.Limits {
		if !strings.EqualFold(limit.Kind, kind) {
			continue
		}
		scoped := ""
		if limit.Scope != nil && limit.Scope.Model != nil {
			scoped = limit.Scope.Model.DisplayName
		}
		if !strings.EqualFold(strings.TrimSpace(scoped), model) {
			continue
		}
		return &UsageWindow{Utilization: limit.Percent, ResetsAt: limit.ResetsAt}
	}
	return nil
}

// FetchOAuthUsage reads the account's current plan utilization.
func (o *ClaudeAuth) FetchOAuthUsage(ctx context.Context, accessToken string) (*Usage, error) {
	body, errFetch := o.fetchOAuthControlPlaneJSON(ctx, UsageURL, accessToken, "usage")
	if errFetch != nil {
		return nil, errFetch
	}
	var usage Usage
	if errDecode := json.Unmarshal(body, &usage); errDecode != nil {
		return nil, fmt.Errorf("parse Claude OAuth usage response: %w", errDecode)
	}
	return &usage, nil
}

// RateLimitHeaders renders the usage as the unified rate-limit headers that
// Anthropic returns on inference responses, so it feeds the same passive quota
// observation path. Only windows with a valid utilization and reset are emitted.
// The session limit stands in for a missing five_hour field, and the
// Fable-scoped weekly limit is reported as the "7d_oi" window that Fable
// responses carry.
func (u *Usage) RateLimitHeaders() http.Header {
	headers := http.Header{}
	if u == nil {
		return headers
	}
	add := func(name string, window *UsageWindow) {
		if window == nil || window.Utilization == nil {
			return
		}
		utilization := *window.Utilization
		if math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 {
			return
		}
		reset, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(window.ResetsAt))
		if errParse != nil {
			return
		}
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		headers.Set(prefix+"Utilization", strconv.FormatFloat(utilization/100, 'f', -1, 64))
		headers.Set(prefix+"Reset", strconv.FormatInt(reset.Unix(), 10))
	}
	fiveHour := u.FiveHour
	if fiveHour == nil || fiveHour.Utilization == nil {
		fiveHour = u.limitWindow("session", "")
	}
	add("5h", fiveHour)
	add("7d", u.SevenDay)
	add("7d_oi", u.limitWindow("weekly_scoped", "Fable"))
	return headers
}
