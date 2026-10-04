package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

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
