package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type serverPolicyFixture struct{}

func (serverPolicyFixture) Discover(context.Context, accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	return accountpolicy.Snapshot{}, nil
}
func (serverPolicyFixture) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	return accountpolicy.ConsumeResult{}, nil
}

func TestServerAccountPolicyRoutesUseAuthenticatedV8Only(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	cfg := &config.Config{Port: 8317}
	server := NewServer(cfg, nil, nil, "", WithRequestLoggerFactory(nil))
	for _, test := range []struct {
		path, key string
		status    int
		code      string
	}{
		{"/v8/management/account-policy/accounts", "", 401, "unauthorized"},
		{"/v8/management/account-policy/accounts", "operator-test", 503, "unavailable"},
		{"/v8/management/account-policy/capabilities", "", 401, "unauthorized"},
		{"/v8/management/account-policy/capabilities", "operator-test", 503, "unavailable"},
		{"/v0/management/account-policy/capabilities", "operator-test", 404, ""},
		{"/v0/management/account-policy/accounts", "operator-test", 404, ""},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		if test.key != "" {
			request.Header.Set("Authorization", "Bearer "+test.key)
		}
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != test.status || test.code != "" && !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
			t.Fatalf("%s: status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}

func TestServerConfirmedResetRouteRequiresManagementAuthentication(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	server := NewServer(&config.Config{Port: 8317}, nil, nil, "", WithRequestLoggerFactory(nil))
	request := httptest.NewRequest(http.MethodPost, "/v8/management/account-policy/resets/redeem-confirmed", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatal(response.Code)
	}
}

func TestServerAccountPolicyPanelRespectsControlPanelSwitch(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, disabled := range []bool{false, true} {
		cfg := &config.Config{Port: 8317}
		cfg.RemoteManagement.DisableControlPanel = disabled
		server := NewServer(cfg, nil, nil, "", WithRequestLoggerFactory(nil))
		for _, path := range []string{"/account-policy.html", "/account-policy.js", "/account-policy.css"} {
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			want := http.StatusOK
			if disabled {
				want = http.StatusNotFound
			}
			if response.Code != want {
				t.Fatalf("disabled=%v %s status=%d", disabled, path, response.Code)
			}
		}
	}
}

func TestServerAccountPolicySettingsRespectLifecycleOwnership(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	settings := accountpolicy.DefaultSettings()
	settings.StateDir = t.TempDir()
	service, err := accountpolicy.NewService(accountpolicy.Options{Settings: settings, Provider: serverPolicyFixture{}, Accounts: func() []accountpolicy.Identity { return nil }, Now: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&config.Config{Port: 8317}, nil, nil, "", WithRequestLoggerFactory(nil), WithAccountPolicy(service), WithAccountPolicySettingsValidator(func(next accountpolicy.Settings) error {
		if next.Enabled {
			return &accountpolicy.Error{Code: "conflict", Message: "another scheduler owns account selection"}
		}
		return nil
	}))
	request := httptest.NewRequest(http.MethodPatch, "/v8/management/account-policy/settings", strings.NewReader(`{"enabled":true}`))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer operator-test")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != 409 || service.Settings().Enabled || !strings.Contains(response.Body.String(), `"code":"conflict"`) {
		t.Fatal(response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("policy response permits caching")
	}
}
