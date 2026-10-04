// account-policy-preview serves sanitized fixtures only. It never loads config,
// auth files, or a provider client, and cannot redeem real credits.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyui"
)

type fixture struct {
	mu                   sync.Mutex
	settings             accountpolicy.Settings
	accounts             []accountpolicy.Snapshot
	operations           []accountpolicy.Operation
	schedules            []accountpolicy.Schedule
	decisions            []accountpolicy.Decision
	requests             int
	failure              string
	now                  time.Time
	clock                func() time.Time
	activeAccounts       map[string]bool
	staleActivity        bool
	dashboardUnavailable bool
	writes               int
	settingsWrites       int
	providerRefreshes    int
	dashboardRequests    int
	snapshotRequests     int
}

func instant(value string) time.Time { t, _ := time.Parse(time.RFC3339, value); return t }
func ptr(value string) *time.Time    { t := instant(value); return &t }

func newFixture() *fixture {
	f := newFixtureAt(time.Now().UTC().Truncate(time.Second))
	f.clock = time.Now
	return f
}

func newFixtureAt(now time.Time) *fixture {
	allowed := true
	f := &fixture{settings: accountpolicy.Settings{Mode: "earliest_deadline", Automation: "off", Fallback: "round-robin", Affinity: "deadline_at_boundary", TimeZone: "Europe/Berlin", ExpiryGuardSeconds: 600, FreshnessSeconds: 120, SavedCreditReserve: 1, CreditTypes: []string{"codex_rate_limits"}, Accounts: map[string]accountpolicy.AccountControl{}}, operations: []accountpolicy.Operation{}, schedules: []accountpolicy.Schedule{}, decisions: []accountpolicy.Decision{}}
	for _, id := range []string{"a", "b"} {
		days := 4
		if id == "b" {
			days = 7
		}
		f.accounts = append(f.accounts, accountpolicy.Snapshot{Identity: accountpolicy.Identity{CredentialID: "account-" + id, AccountID: "upstream-" + id, WorkspaceID: "workspace-" + id, Alias: "Account " + strings.ToUpper(id), Provider: "codex", Generation: 1}, Version: 1, Plan: "Plus", Status: "healthy", Eligible: true, InventoryComplete: true, ObservedAt: now, InventoryObservedAt: now, Transport: "fixture HTTP", ActiveBindings: 2, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 18000, UsedPercent: 34, Allowed: &allowed, ResetAt: now.Add(3 * time.Hour), ObservedAt: now, Source: "sanitized_fixture"}, {Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 67, Allowed: &allowed, ResetAt: now.Add(time.Duration(days) * 24 * time.Hour), ObservedAt: now, Source: "sanitized_fixture"}}, Credits: []accountpolicy.Credit{}})
	}
	f.accounts[1].AvailableCredits = 3
	f.accounts[1].Credits = []accountpolicy.Credit{{ID: "october-05", Type: "codex_rate_limits", Status: "available", Title: "Reset 1", ExpiresAt: ptr("2026-10-05T04:18:00Z"), DetailsKnown: true, Scopes: []string{"ordinary"}}, {ID: "october-22", Type: "codex_rate_limits", Status: "available", Title: "Reset 2", ExpiresAt: ptr("2026-10-22T20:27:00Z"), DetailsKnown: true, Scopes: []string{"ordinary"}}, {ID: "october-29", Type: "codex_rate_limits", Status: "available", Title: "Reset 3", ExpiresAt: ptr("2026-10-29T17:48:00Z"), DetailsKnown: true, Scopes: []string{"ordinary"}}}
	// Deliberately unsorted inventory proves display sorting never changes selection.
	f.accounts[1].Credits = []accountpolicy.Credit{f.accounts[1].Credits[2], f.accounts[1].Credits[0], f.accounts[1].Credits[1]}
	unknown := f.accounts[0]
	unknown.Identity = accountpolicy.Identity{CredentialID: "inventory-c", AccountID: "upstream-c", WorkspaceID: "workspace-c", Alias: "Inventory edge cases", Provider: "codex", Generation: 1}
	unknown.ObservedAt = now.Add(-10 * time.Minute)
	unknown.InventoryObservedAt = unknown.ObservedAt
	unknown.InventoryComplete = false
	unknown.AvailableCredits = 4
	unknown.ActiveBindings = 0
	unknown.LastError = "Fixture: last observation failed; prior inventory retained"
	unknown.Credits = []accountpolicy.Credit{{ID: "unknown-expiry", Type: "codex_rate_limits", Status: "available", Title: "Incomplete reset details", DetailsKnown: false}, {ID: "saved-forever", Type: "codex_rate_limits", Status: "available", Title: "Saved reset", DetailsKnown: true, Scopes: []string{"ordinary"}}, {ID: "expired-credit", Type: "codex_rate_limits", Status: "expired", Title: "Expired reset", ExpiresAt: ptr("2026-10-01T10:00:00Z"), DetailsKnown: true, Scopes: []string{"ordinary"}}}
	f.accounts = append(f.accounts, unknown)
	f.operations = append(f.operations, accountpolicy.Operation{ID: "expired-history", RequestID: "fixture-expired-request", CredentialID: "inventory-c", AccountID: "upstream-c", WorkspaceID: "workspace-c", CreditID: "expired-credit", State: "expired", CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour)})
	f.decisions = append(f.decisions, accountpolicy.Decision{CredentialID: "account-a", Provider: "codex", Model: "gpt-5.4", Reason: "Fixture: policy disabled; existing round-robin strategy", At: now, Fallback: true})
	f.now = now
	f.clock = func() time.Time { return now }
	f.activeAccounts = map[string]bool{"account-a": true}
	f.applyActivity()
	return f
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (f *fixture) handler() http.Handler {
	static := accountpolicyui.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/account-policy.html", http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/management.html" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<h1>Sanitized fixture preview</h1><p>This preview never connects to a provider. Management key: fixture-key.</p><a href="/account-policy.html">Open policy controls</a>`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/account-policy.") || r.URL.Path == "/account-policy-dashboard.js" || r.URL.Path == "/account-policy-icon.svg" {
			static.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			failure(w, 401, "unauthorized", "Use fixture-key in this sanitized preview.")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/fixture/state" {
			write(w, 200, map[string]any{"fixture_only": true, "requests": f.requests, "accounts": f.accounts, "operations": f.operations, "settings": f.settings, "account_count": len(f.accounts), "writes": f.writes, "settings_writes": f.settingsWrites, "provider_refreshes": f.providerRefreshes, "dashboard_requests": f.dashboardRequests, "snapshot_requests": f.snapshotRequests})
			return
		}
		if r.URL.Path == "/fixture/control" && r.Method == "POST" {
			var body fixtureControl
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				failure(w, 400, "invalid_json", "Invalid fixture control.")
				return
			}
			if err := f.validateControl(body); err != nil {
				failure(w, 400, "invalid_control", err.Error())
				return
			}
			if body.Reset {
				other := newFixtureAt(f.now)
				f.settings = other.settings
				f.accounts = other.accounts
				f.operations = other.operations
				f.schedules = other.schedules
				f.decisions = other.decisions
				f.activeAccounts = other.activeAccounts
				f.staleActivity = false
				f.dashboardUnavailable = false
				f.requests, f.writes, f.settingsWrites, f.providerRefreshes, f.dashboardRequests, f.snapshotRequests = 0, 0, 0, 0, 0, 0
			}
			if body.AccountCount != nil {
				f.expandAccounts(*body.AccountCount)
			}
			if body.ActiveAccounts != nil {
				f.activeAccounts = make(map[string]bool)
				for _, id := range *body.ActiveAccounts {
					f.activeAccounts[id] = true
				}
			}
			if body.StaleActivity != nil {
				f.staleActivity = *body.StaleActivity
			}
			if body.DashboardUnavailable != nil {
				f.dashboardUnavailable = *body.DashboardUnavailable
			}
			f.applyActivity()
			f.failure = body.Failure
			if body.NearExpiry {
				expiry := f.clock().UTC().Add(45 * time.Second)
				_, credit := f.selected("account-b", "october-05")
				credit.ExpiresAt = &expiry
			}
			write(w, 200, map[string]bool{"fixture_only": true})
			return
		}
		f.requests++
		if f.failure != "" && r.Method != "GET" {
			code := f.failure
			f.failure = ""
			failure(w, 409, code, "Sanitized fixture injected operation failure.")
			return
		}
		if r.URL.Path == "/v8/management/credentials" && r.Method == "GET" {
			files := make([]map[string]string, 0, len(f.accounts))
			for _, account := range f.accounts {
				index := strings.TrimPrefix(account.Identity.CredentialID, "account-")
				if index == "inventory-c" {
					index = "c"
				}
				files = append(files, map[string]string{"id": account.Identity.CredentialID, "auth_index": "fixture-" + index})
			}
			write(w, 200, map[string]any{"files": files})
			return
		}
		if r.URL.Path == "/v8/management/routing/cooldown/reset" && r.Method == "POST" {
			f.writes++
			write(w, 200, map[string]string{"status": "ok", "fixture_only": "true"})
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v8/management/account-policy")
		switch {
		case path == "/dashboard" && r.Method == "GET":
			f.dashboardRequests++
			if f.dashboardUnavailable {
				failure(w, 503, "fixture_dashboard_unavailable", "Sanitized fixture telemetry is temporarily unavailable.")
				return
			}
			value, err := f.dashboard(r.URL.Query().Get("range"))
			if err != nil {
				failure(w, 400, "invalid_range", err.Error())
				return
			}
			write(w, 200, value)
		case path == "/accounts" && r.Method == "GET":
			f.snapshotRequests++
			write(w, 200, map[string]any{"settings": f.settings, "accounts": f.accounts, "fixture_only": true})
		case path == "/decisions" && r.Method == "GET":
			write(w, 200, map[string]any{"decisions": f.decisions})
		case path == "/resets" && r.Method == "GET":
			write(w, 200, map[string]any{"operations": f.operations, "schedules": f.schedules})
		case path == "/diagnostics" && r.Method == "GET":
			write(w, 200, map[string]any{"settings": f.settings, "accounts": f.accounts, "operations": f.operations, "schedules": f.schedules, "decisions": f.decisions, "fixture_only": true})
		case path == "/settings" && r.Method == "PATCH":
			if err := json.NewDecoder(r.Body).Decode(&f.settings); err != nil {
				failure(w, 400, "invalid_json", err.Error())
				return
			}
			f.writes++
			f.settingsWrites++
			write(w, 200, map[string]any{"settings": f.settings})
		case path == "/refresh" && r.Method == "POST":
			f.writes++
			f.providerRefreshes++
			for i := range f.accounts {
				f.accounts[i].ObservedAt = f.clock().UTC()
				f.accounts[i].InventoryObservedAt = f.accounts[i].ObservedAt
				f.accounts[i].LastError = ""
			}
			write(w, 200, map[string]any{"accounts": f.accounts})
		case path == "/resets/redeem" && r.Method == "POST":
			var body struct {
				CredentialID string `json:"credential_id"`
				CreditID     string `json:"credit_id"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				failure(w, 400, "invalid_json", "Invalid redeem request.")
				return
			}
			a, c := f.selected(body.CredentialID, body.CreditID)
			if a == nil || c == nil {
				failure(w, 409, "credit_unavailable", "Credit does not belong to the selected account.")
				return
			}
			if !f.settings.Enabled || f.settings.ReadOnly || a.WritesDisabled || c.Status != "available" || !c.DetailsKnown || c.ExpiresAt != nil && !c.ExpiresAt.After(f.clock()) {
				failure(w, 409, "write_disabled", "Selected credit cannot be redeemed.")
				return
			}
			now := f.clock().UTC()
			operation := accountpolicy.Operation{ID: fmt.Sprintf("fixture-op-%d", len(f.operations)+1), RequestID: fmt.Sprintf("fixture-request-%d", len(f.operations)+1), CredentialID: a.Identity.CredentialID, AccountID: a.Identity.AccountID, WorkspaceID: a.Identity.WorkspaceID, CreditID: c.ID, State: "confirmed", Result: "reset", CreatedAt: now, UpdatedAt: now}
			c.Status = "redeemed"
			a.AvailableCredits--
			a.Version++
			for i := range a.Buckets {
				a.Buckets[i].UsedPercent = 2
				a.Buckets[i].ResetAt = now.Add(time.Duration(a.Buckets[i].DurationSeconds) * time.Second)
			}
			a.ObservedAt = now
			a.InventoryObservedAt = now
			f.operations = append(f.operations, operation)
			f.writes++
			write(w, 200, map[string]any{"operation": operation})
		case path == "/resets/schedule" && r.Method == "POST":
			var body accountpolicy.Schedule
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				failure(w, 400, "invalid_json", err.Error())
				return
			}
			a, c := f.selected(body.CredentialID, body.CreditID)
			if a == nil || c == nil || !body.At.After(f.clock()) || c.ExpiresAt != nil && !body.At.Before(*c.ExpiresAt) {
				failure(w, 409, "schedule_invalid", "Schedule must reference the owning account credit and precede its expiry.")
				return
			}
			body.ID = fmt.Sprintf("fixture-schedule-%d", len(f.schedules)+1)
			f.schedules = append(f.schedules, body)
			f.writes++
			write(w, 200, map[string]any{"schedule": body})
		case strings.HasPrefix(path, "/resets/schedule/") && r.Method == "DELETE":
			id := strings.TrimPrefix(path, "/resets/schedule/")
			for i, s := range f.schedules {
				if s.ID == id {
					f.schedules = append(f.schedules[:i], f.schedules[i+1:]...)
					f.writes++
					write(w, 200, map[string]bool{"cancelled": true})
					return
				}
			}
			failure(w, 404, "schedule_not_found", "Schedule not found.")
		default:
			failure(w, 404, "not_found", "Unknown fixture route.")
		}
	})
}

func (f *fixture) selected(account, credit string) (*accountpolicy.Snapshot, *accountpolicy.Credit) {
	for i := range f.accounts {
		a := &f.accounts[i]
		if a.Identity.CredentialID != account {
			continue
		}
		for j := range a.Credits {
			if a.Credits[j].ID == credit {
				return a, &a.Credits[j]
			}
		}
	}
	return nil, nil
}

func validateListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	_, tailnet, _ := net.ParseCIDR("100.64.0.0/10")
	if ip == nil || (!ip.IsLoopback() && !tailnet.Contains(ip)) {
		return errors.New("preview listener must be an explicit loopback or Tailscale IP")
	}
	return nil
}

func main() {
	addr := flag.String("listen", "127.0.0.1:8318", "explicit loopback or Tailscale fixture listener")
	flag.Parse()
	if err := validateListen(*addr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("SANITIZED FIXTURE ONLY — no provider credentials or live redemption.\nPreview: http://%s/account-policy.html\nManagement key: fixture-key\n", *addr)
	server := &http.Server{Addr: *addr, Handler: newFixture().handler()}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
