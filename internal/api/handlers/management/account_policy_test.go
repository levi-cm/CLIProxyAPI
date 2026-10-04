package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type policyFixtureProvider struct{}

func (policyFixtureProvider) Discover(_ context.Context, id accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	return accountpolicy.Snapshot{Identity: id, Eligible: true, InventoryComplete: true, ObservedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), InventoryObservedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), LastError: "Bearer secret-token prompt-secret", Credits: []accountpolicy.Credit{{ID: "c", Type: "codex_rate_limits", Status: "available", DetailsKnown: true, Scopes: []string{"ordinary"}}}}, nil
}
func (policyFixtureProvider) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	return accountpolicy.ConsumeResult{Code: "nothing_to_reset"}, nil
}

func policyTestRouter(t *testing.T, enabled bool) (*gin.Engine, *accountpolicy.Service) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	h := NewHandler(&config.Config{}, "", nil)
	var service *accountpolicy.Service
	if enabled {
		settings := accountpolicy.DefaultSettings()
		settings.Enabled = true
		settings.StateDir = t.TempDir()
		var err error
		service, err = accountpolicy.NewService(accountpolicy.Options{Settings: settings, Provider: policyFixtureProvider{}, Accounts: func() []accountpolicy.Identity {
			return []accountpolicy.Identity{{CredentialID: "a", AccountID: "upstream-a", WorkspaceID: "workspace-a", Provider: "codex"}}
		}, Now: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }, IsIdle: func(string) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		h.SetAccountPolicy(service)
	}
	r := gin.New()
	g := r.Group("/v8/management/account-policy", h.AccountPolicyMiddleware())
	g.GET("/accounts", h.GetAccountPolicyAccounts)
	g.PATCH("/settings", h.PatchAccountPolicySettings)
	g.POST("/refresh", h.RefreshAccountPolicy)
	g.POST("/resets/redeem", h.RedeemAccountPolicyReset)
	g.POST("/resets/schedule", h.ScheduleAccountPolicyReset)
	g.DELETE("/resets/schedule/:schedule_id", h.CancelAccountPolicySchedule)
	g.GET("/diagnostics", h.GetAccountPolicyDiagnostics)
	return r, service
}
func policyRequest(r http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v8/management/account-policy"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestAccountPolicyAuthenticationAndUnavailable(t *testing.T) {
	r, _ := policyTestRouter(t, false)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/accounts"
		if method == http.MethodPost {
			path = "/resets/redeem"
		}
		w := policyRequest(r, method, path, `{}`, "")
		if w.Code != 401 || !strings.Contains(w.Body.String(), `"code":"unauthorized"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := policyRequest(r, http.MethodGet, "/accounts", "", "operator-test")
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"unavailable"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestAccountPolicyPartialSettingsAndInvalidPatch(t *testing.T) {
	r, service := policyTestRouter(t, true)
	before := service.Settings()
	w := policyRequest(r, http.MethodPatch, "/settings", `{"time_zone":"UTC"}`, "operator-test")
	if w.Code != 200 || service.Settings().Automation != before.Automation || service.Settings().TimeZone != "UTC" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"automation":"unexpected"}`, `{"unknown":true}`, `{"enabled":null}`, `{} {}`} {
		w = policyRequest(r, http.MethodPatch, "/settings", body, "operator-test")
		if w.Code != 400 || service.Settings().TimeZone != "UTC" || service.Settings().Automation != before.Automation {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestAccountPolicyExplicitRedemptionAndSchedule(t *testing.T) {
	r, _ := policyTestRouter(t, true)
	for _, body := range []string{`{}`, `{"credential_id":"a"}`, `{"credit_id":"c"}`} {
		w := policyRequest(r, http.MethodPost, "/resets/redeem", body, "operator-test")
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := policyRequest(r, http.MethodPost, "/resets/schedule", `{"credential_id":"a","credit_id":"c","at":"2026-10-25T01:30:00"}`, "operator-test")
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestAccountPolicyDiagnosticsSanitizeProviderErrors(t *testing.T) {
	r, service := policyTestRouter(t, true)
	if err := service.Refresh(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	w := policyRequest(r, http.MethodGet, "/diagnostics", "", "operator-test")
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "secret-token") || strings.Contains(w.Body.String(), "prompt-secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	if snapshot, ok := service.Snapshot("a"); !ok || snapshot.LastError != "Bearer secret-token prompt-secret" {
		t.Fatal("diagnostic export changed stored provider evidence")
	}
}

func TestAccountPolicyConcurrentPatchesPreserveIndependentControls(t *testing.T) {
	r, service := policyTestRouter(t, true)
	var wg sync.WaitGroup
	for _, body := range []string{`{"accounts":{"a":{"hold":true}}}`, `{"accounts":{"a":{"reserve_percent":25}}}`} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			response := policyRequest(r, http.MethodPatch, "/settings", body, "operator-test")
			if response.Code != 200 {
				t.Errorf("patch failed %d %s", response.Code, response.Body.String())
			}
		}(body)
	}
	wg.Wait()
	control := service.Settings().Accounts["a"]
	if !control.Hold || control.ReservePercent != 25 {
		t.Fatalf("lost concurrent control: %+v", control)
	}
}

func TestAccountPolicyInvalidAccountPatchDoesNotMutateSettings(t *testing.T) {
	r, service := policyTestRouter(t, true)
	response := policyRequest(r, http.MethodPatch, "/settings", `{"accounts":{"a":{"hold":true,"reserve_percent":101}}}`, "operator-test")
	if response.Code != 400 || service.Settings().Accounts["a"].Hold {
		t.Fatalf("invalid patch mutated settings: %d %s", response.Code, response.Body.String())
	}
}

func TestAccountPolicyRejectsNonObjectAndOversizedRequests(t *testing.T) {
	r, _ := policyTestRouter(t, true)
	for _, body := range []string{"null", "[]", `"a"`, `{"credential_id":"a","unknown":true}`, `{"credential_id":"` + strings.Repeat("a", 65<<10) + `"}`} {
		response := policyRequest(r, http.MethodPost, "/refresh", body, "operator-test")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("unexpected status=%d response=%s", response.Code, response.Body.String())
		}
	}
}

func TestAccountPolicyScheduleRetainsInstantAndCancel(t *testing.T) {
	r, service := policyTestRouter(t, true)
	if err := service.Refresh(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	w := policyRequest(r, http.MethodPost, "/resets/schedule", `{"credential_id":"a","credit_id":"c","at":"2026-10-25T01:30:00+02:00"}`, "operator-test")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	schedules := service.Schedules()
	if len(schedules) != 1 || schedules[0].At.Format(time.RFC3339) != "2026-10-24T23:30:00Z" {
		t.Fatalf("wrong instant: %+v", schedules)
	}
	w = policyRequest(r, http.MethodDelete, "/resets/schedule/"+schedules[0].ID, "", "operator-test")
	if w.Code != 200 || len(service.Schedules()) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAccountPolicyRedeemReturnsSelectedOperation(t *testing.T) {
	r, _ := policyTestRouter(t, true)
	w := policyRequest(r, http.MethodPost, "/resets/redeem", `{"credential_id":"a","credit_id":"c"}`, "operator-test")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Operation accountpolicy.Operation `json:"operation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Operation.CredentialID != "a" || result.Operation.CreditID != "c" || result.Operation.State != "nothing_to_reset" || result.Operation.RequestID == "" {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-token") {
		t.Fatal("operation leaked upstream diagnostics")
	}
}

func TestAccountPolicyPersistenceFailureRollsBackPatch(t *testing.T) {
	r, service := policyTestRouter(t, true)
	stateDir := service.Settings().StateDir
	// A directory at the journal destination forces persistence to fail on every platform.
	if err := os.Mkdir(filepath.Join(stateDir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	w := policyRequest(r, http.MethodPatch, "/settings", `{"time_zone":"UTC"}`, "operator-test")
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"code":"persistence"`) || service.Settings().TimeZone != "Europe/Berlin" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAccountPolicyDisabledAndReadOnlyRejectRedemption(t *testing.T) {
	for _, test := range []struct {
		patch, code string
		status      int
	}{{`{"enabled":false}`, "disabled", 503}, {`{"read_only":true}`, "read_only", 403}} {
		t.Run(test.code, func(t *testing.T) {
			r, service := policyTestRouter(t, true)
			if response := policyRequest(r, http.MethodPatch, "/settings", test.patch, "operator-test"); response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			response := policyRequest(r, http.MethodPost, "/resets/redeem", `{"credential_id":"a","credit_id":"c"}`, "operator-test")
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) || len(service.Operations()) != 0 {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
}
