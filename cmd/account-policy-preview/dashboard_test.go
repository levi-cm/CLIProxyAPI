package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func previewRequest(t *testing.T, h http.Handler, method, path, body string, wantStatus int) map[string]any {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer fixture-key")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != wantStatus {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, recorder.Code, wantStatus, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return response
}

func TestPreviewDashboardRequiresAuthentication(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest("GET", "/v8/management/account-policy/dashboard?range=1h", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard status=%d", recorder.Code)
	}
	previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard?range=1h", "", http.StatusOK)
}

func TestPreviewDashboardRangeHistoryIsAvailableWhenPolicyDisabled(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	for _, test := range []struct {
		rangeName string
		seconds   float64
	}{
		{"1h", 3600}, {"24h", 86400}, {"7d", 604800},
	} {
		t.Run(test.rangeName, func(t *testing.T) {
			response := previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard?range="+test.rangeName, "", http.StatusOK)
			if response["fixture_only"] != true {
				t.Fatal("dashboard does not label fixture-only data")
			}
			activity := response["activity"].(map[string]any)
			usage := response["usage"].(map[string]any)
			if activity["available"] != true || usage["available"] != true || usage["collecting"] != true || usage["fixture_only"] != true {
				t.Fatalf("disabled policy hid fixture telemetry: %v", response)
			}
			if usage["range_seconds"] != test.seconds {
				t.Fatalf("range_seconds=%v want=%v", usage["range_seconds"], test.seconds)
			}
			start, errStart := time.Parse(time.RFC3339, usage["coverage_start"].(string))
			end, errEnd := time.Parse(time.RFC3339, usage["coverage_end"].(string))
			if errStart != nil || errEnd != nil || end.Sub(start).Seconds() != test.seconds {
				t.Fatalf("incorrect retained range: %v", usage)
			}
			totals := usage["totals"].(map[string]any)
			if totals["requests"].(float64) <= 0 || totals["total_tokens"].(float64) <= 0 || totals["average_latency_ms"] == nil {
				t.Fatalf("missing representative totals: %v", totals)
			}
			var requests, successes, failed, tokens, input, output float64
			var previous time.Time
			for _, entry := range usage["series"].([]any) {
				point := entry.(map[string]any)
				at, err := time.Parse(time.RFC3339, point["at"].(string))
				if err != nil || at.Before(start) || !at.Before(end) || !previous.IsZero() && !at.After(previous) {
					t.Fatalf("unordered or out-of-range series point: %v", point)
				}
				previous = at
				requests += point["requests"].(float64)
				successes += point["success"].(float64)
				failed += point["failed"].(float64)
				tokens += point["total_tokens"].(float64)
				input += point["input_tokens"].(float64)
				output += point["output_tokens"].(float64)
			}
			if requests != totals["requests"] || successes != totals["success"] || failed != totals["failed"] || tokens != totals["total_tokens"] || input != totals["input_tokens"] || output != totals["output_tokens"] || successes+failed != requests {
				t.Fatalf("series does not reconcile with totals: %v", usage)
			}
		})
	}
	previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard?range=30d", "", http.StatusBadRequest)
	previewRequest(t, h, "POST", "/v8/management/account-policy/dashboard", `{}`, http.StatusNotFound)
}

func TestPreviewControlExpandsPoolAndDistinguishesConcurrentActivityFromIdle(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	previewRequest(t, h, "POST", "/fixture/control", `{"account_count":100,"active_accounts":["account-a","account-100"],"stale_activity":true}`, http.StatusOK)
	response := previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard?range=1h", "", http.StatusOK)
	activity := response["activity"].(map[string]any)["accounts"].([]any)
	usage := response["usage"].(map[string]any)["accounts"].([]any)
	if len(activity) != 100 || len(usage) != 100 {
		t.Fatalf("expanded activity=%d usage=%d", len(activity), len(usage))
	}
	seen := make(map[string]bool)
	for _, entry := range activity {
		account := entry.(map[string]any)
		id := account["credential_id"].(string)
		if seen[id] || account["alias"] == "" || account["provider"] == "" || account["disabled"] != false {
			t.Fatalf("invalid expanded account: %v", account)
		}
		seen[id] = true
		if id == "account-a" || id == "account-100" {
			if account["active_requests"].(float64) <= 0 || account["active_bindings"].(float64) <= 0 || account["last_selected_at"] == nil {
				t.Fatalf("active account not represented: %v", account)
			}
		} else if account["active_requests"] != float64(0) || account["active_bindings"] != float64(0) {
			t.Fatalf("idle account falsely active: %v", account)
		}
		if id == "account-b" {
			selected, err := time.Parse(time.RFC3339, account["last_selected_at"].(string))
			sampled, _ := time.Parse(time.RFC3339, response["sampled_at"].(string))
			if err != nil || sampled.Sub(selected) != 48*time.Hour {
				t.Fatalf("stale last selection not separate from current activity: %v", account)
			}
		}
	}
	for _, entry := range usage {
		if !seen[entry.(map[string]any)["credential_id"].(string)] {
			t.Fatalf("usage references unknown credential: %v", entry)
		}
	}
	previewRequest(t, h, "POST", "/fixture/control", `{"account_count":2,"active_accounts":[]}`, http.StatusOK)
	response = previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard", "", http.StatusOK)
	if len(response["activity"].(map[string]any)["accounts"].([]any)) != 2 {
		t.Fatal("minimum expansion did not retain exactly the two base accounts")
	}
}

func TestPreviewControlRejectsInvalidPoolWithoutChangingState(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	before := previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
	for _, body := range []string{`{"account_count":1}`, `{"account_count":101}`, `{"account_count":2.5}`, `{"active_accounts":["missing"]}`, `{"account_count":2,"active_accounts":["inventory-c"]}`} {
		previewRequest(t, h, "POST", "/fixture/control", body, http.StatusBadRequest)
		after := previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected control changed fixture state: %s", body)
		}
	}
}

func TestPreviewDashboardPollingDoesNotRefreshProviderOrWriteSettings(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	before := previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
	for range 3 {
		previewRequest(t, h, "GET", "/v8/management/account-policy/accounts", "", http.StatusOK)
		previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard?range=24h", "", http.StatusOK)
	}
	after := previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
	for _, name := range []string{"writes", "settings_writes", "provider_refreshes"} {
		if before[name] != float64(0) || after[name] != float64(0) {
			t.Fatalf("polling mutated %s before=%v after=%v", name, before[name], after[name])
		}
	}
	if after["dashboard_requests"] != float64(3) || after["snapshot_requests"] != float64(3) || !reflect.DeepEqual(before["settings"], after["settings"]) || !reflect.DeepEqual(before["accounts"], after["accounts"]) {
		t.Fatalf("polling did not preserve snapshot state: %v", after)
	}
	previewRequest(t, h, "PATCH", "/v8/management/account-policy/settings", `{"enabled":true}`, http.StatusOK)
	previewRequest(t, h, "POST", "/v8/management/account-policy/refresh", `{}`, http.StatusOK)
	after = previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
	if after["writes"] != float64(2) || after["settings_writes"] != float64(1) || after["provider_refreshes"] != float64(1) {
		t.Fatalf("explicit writes not observable: %v", after)
	}
}

func TestPreviewDashboardCanSimulateUnavailableTelemetryWithoutLosingInventory(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	previewRequest(t, h, "POST", "/fixture/control", `{"dashboard_unavailable":true}`, http.StatusOK)
	previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard", "", http.StatusServiceUnavailable)
	response := previewRequest(t, h, "GET", "/v8/management/account-policy/accounts", "", http.StatusOK)
	if len(response["accounts"].([]any)) != 3 {
		t.Fatal("dashboard outage hid the retained inventory")
	}
	previewRequest(t, h, "POST", "/fixture/control", `{"dashboard_unavailable":false}`, http.StatusOK)
	previewRequest(t, h, "GET", "/v8/management/account-policy/dashboard", "", http.StatusOK)
}

func TestPreviewControlResetPreservesOriginalInventoryAndOperations(t *testing.T) {
	h := newFixtureAt(instant("2026-10-04T12:00:00Z")).handler()
	before := previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
	previewRequest(t, h, "POST", "/fixture/control", `{"account_count":10,"active_accounts":["account-a"],"near_expiry":true,"failure":"fixture_failure"}`, http.StatusOK)
	previewRequest(t, h, "POST", "/fixture/control", `{"reset":true}`, http.StatusOK)
	after := previewRequest(t, h, "GET", "/fixture/state", "", http.StatusOK)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("reset did not restore original fixture state: before=%v after=%v", before, after)
	}
	accounts := after["accounts"].([]any)
	if len(accounts) != 3 || accounts[0].(map[string]any)["identity"].(map[string]any)["credential_id"] != "account-a" || accounts[1].(map[string]any)["available_credits"] != float64(3) {
		t.Fatalf("original account inventory lost: %v", accounts)
	}
	credits := accounts[1].(map[string]any)["credits"].([]any)
	wantExpiry := map[string]string{"october-05": "2026-10-05T04:18:00Z", "october-22": "2026-10-22T20:27:00Z", "october-29": "2026-10-29T17:48:00Z"}
	for _, entry := range credits {
		credit := entry.(map[string]any)
		if credit["expires_at"] != wantExpiry[credit["id"].(string)] {
			t.Fatalf("original credit expiry lost: %v", credit)
		}
	}
}
