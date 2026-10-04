package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

type fixtureControl struct {
	Failure              string    `json:"failure"`
	NearExpiry           bool      `json:"near_expiry"`
	Reset                bool      `json:"reset"`
	AccountCount         *int      `json:"account_count"`
	ActiveAccounts       *[]string `json:"active_accounts"`
	StaleActivity        *bool     `json:"stale_activity"`
	DashboardUnavailable *bool     `json:"dashboard_unavailable"`
}

func fixtureAccountID(index int) string {
	switch index {
	case 0:
		return "account-a"
	case 1:
		return "account-b"
	case 2:
		return "inventory-c"
	default:
		return fmt.Sprintf("account-%03d", index+1)
	}
}

func (f *fixture) validateControl(body fixtureControl) error {
	count := len(f.accounts)
	if body.Reset {
		count = 3
	}
	if body.AccountCount != nil {
		count = *body.AccountCount
		if count < 2 || count > 100 {
			return errors.New("fixture account_count must be an integer from 2 through 100")
		}
	}
	if body.ActiveAccounts != nil {
		known := make(map[string]bool, count)
		for i := range count {
			known[fixtureAccountID(i)] = true
		}
		for _, id := range *body.ActiveAccounts {
			if !known[id] {
				return fmt.Errorf("fixture active account %q is outside the selected pool", id)
			}
		}
	}
	return nil
}

func (f *fixture) expandAccounts(count int) {
	if count <= len(f.accounts) {
		f.accounts = f.accounts[:count]
		return
	}
	base := newFixtureAt(f.now)
	providers := []string{"codex", "claude", "gemini"}
	for len(f.accounts) < count {
		i := len(f.accounts)
		if i == 2 {
			f.accounts = append(f.accounts, base.accounts[2])
			continue
		}
		id := fixtureAccountID(i)
		provider := providers[i%len(providers)]
		alias := fmt.Sprintf("Fixture %s account %03d", strings.ToUpper(provider[:1])+provider[1:], i+1)
		if i == 3 {
			alias = "Fixture research workspace with a deliberately long account alias for narrow screens and large pools"
		}
		account := accountpolicy.Snapshot{Identity: accountpolicy.Identity{CredentialID: id, AccountID: "fixture-upstream-" + id, WorkspaceID: "fixture-workspace-" + id, Alias: alias, Provider: provider, Generation: 1}, Version: 1, Plan: "Fixture", Status: "healthy", Eligible: true, InventoryComplete: true, ObservedAt: f.now, InventoryObservedAt: f.now, Transport: "fixture HTTP", Buckets: []accountpolicy.Bucket{}, Credits: []accountpolicy.Credit{}}
		if provider == "codex" {
			account.Buckets = append([]accountpolicy.Bucket(nil), base.accounts[0].Buckets...)
			for j := range account.Buckets {
				account.Buckets[j].UsedPercent = float64((i*11 + j*17) % 90)
			}
		}
		f.accounts = append(f.accounts, account)
	}
}

func (f *fixture) applyActivity() {
	for i := range f.accounts {
		account := &f.accounts[i]
		account.ActiveRequests = 0
		account.ActiveBindings = 0
		if f.activeAccounts[account.Identity.CredentialID] {
			account.ActiveRequests = 2
			account.ActiveBindings = 2
			if i%2 == 0 {
				account.Transport = "fixture WebSocket"
			} else {
				account.Transport = "fixture HTTP"
			}
		}
	}
}

// dashboard builds illustrative retained usage, never provider observations.
// Every read of the same fixture state returns the same buckets and totals.
func (f *fixture) dashboard(rangeName string) (map[string]any, error) {
	var rangeSeconds, bucketSeconds int
	switch rangeName {
	case "1h":
		rangeSeconds, bucketSeconds = 3600, 300
	case "", "24h":
		rangeSeconds, bucketSeconds = 86400, 3600
	case "7d":
		rangeSeconds, bucketSeconds = 604800, 21600
	default:
		return nil, errors.New("dashboard range must be 1h, 24h, or 7d")
	}
	sampled := f.clock().UTC().Truncate(time.Second)
	if f.staleActivity {
		sampled = sampled.Add(-10 * time.Minute)
	}
	start := sampled.Add(-time.Duration(rangeSeconds) * time.Second)
	activity := make([]map[string]any, 0, len(f.accounts))
	accountUsage := make([]map[string]any, 0, len(f.accounts))
	series := make([]map[string]any, rangeSeconds/bucketSeconds)
	for i := range series {
		series[i] = map[string]any{"at": start.Add(time.Duration(i*bucketSeconds) * time.Second), "requests": 0, "success": 0, "failed": 0, "total_tokens": 0, "input_tokens": 0, "output_tokens": 0, "average_latency_ms": nil}
	}
	totalRequests, totalSuccess, totalFailed, totalTokens, totalInput, totalOutput, totalReasoning := 0, 0, 0, 0, 0, 0, 0
	for index, account := range f.accounts {
		var selected *time.Time
		if account.ActiveRequests > 0 || account.ActiveBindings > 0 {
			at := sampled.Add(-15 * time.Second)
			selected = &at
		} else if account.Identity.CredentialID == "account-b" {
			at := sampled.Add(-30 * time.Minute)
			if f.staleActivity {
				at = sampled.Add(-48 * time.Hour)
			}
			selected = &at
		}
		activity = append(activity, map[string]any{"credential_id": account.Identity.CredentialID, "alias": account.Identity.Alias, "provider": account.Identity.Provider, "active_requests": account.ActiveRequests, "active_bindings": account.ActiveBindings, "transport": account.Transport, "last_selected_at": selected, "status": account.Status, "disabled": false})
		requests, success, failed, tokens := 0, 0, 0, 0
		for bucketIndex, bucket := range series {
			// Handcrafted, reproducible fixture shape includes peaks and failures.
			count := 1 + (bucketIndex+index*3)%7
			failures := 0
			if (bucketIndex+index)%9 == 0 {
				failures = 1
			}
			input := count * (120 + index*4)
			output := count * (60 + index*2)
			reasoning := count * 20
			tokenCount := input + output + reasoning
			bucket["requests"] = bucket["requests"].(int) + count
			bucket["success"] = bucket["success"].(int) + count - failures
			bucket["failed"] = bucket["failed"].(int) + failures
			bucket["input_tokens"] = bucket["input_tokens"].(int) + input
			bucket["output_tokens"] = bucket["output_tokens"].(int) + output
			bucket["total_tokens"] = bucket["total_tokens"].(int) + tokenCount
			bucket["average_latency_ms"] = 240.0
			requests += count
			success += count - failures
			failed += failures
			tokens += tokenCount
			totalInput += input
			totalOutput += output
			totalReasoning += reasoning
		}
		totalRequests += requests
		totalSuccess += success
		totalFailed += failed
		totalTokens += tokens
		accountUsage = append(accountUsage, map[string]any{"credential_id": account.Identity.CredentialID, "requests": requests, "success": success, "failed": failed, "total_tokens": tokens, "average_latency_ms": 240.0})
	}
	return map[string]any{
		"fixture_only": true,
		"sampled_at":   sampled,
		"activity":     map[string]any{"available": true, "accounts": activity},
		"usage":        map[string]any{"fixture_only": true, "available": true, "collecting": true, "coverage_start": start, "coverage_end": sampled, "range_seconds": rangeSeconds, "bucket_seconds": bucketSeconds, "totals": map[string]any{"requests": totalRequests, "success": totalSuccess, "failed": totalFailed, "total_tokens": totalTokens, "input_tokens": totalInput, "output_tokens": totalOutput, "reasoning_tokens": totalReasoning, "average_latency_ms": 240.0}, "series": series, "accounts": accountUsage},
	}, nil
}
