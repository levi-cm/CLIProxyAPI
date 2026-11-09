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

// Usage holds the plan windows the router understands. A nil window means the
// account currently has no such window open.
type Usage struct {
	FiveHour *UsageWindow `json:"five_hour"`
	SevenDay *UsageWindow `json:"seven_day"`
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
	add("5h", u.FiveHour)
	add("7d", u.SevenDay)
	return headers
}
