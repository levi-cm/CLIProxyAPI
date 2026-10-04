package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Missing route authentication would expose local credential activity.
func TestServerAccountPolicyDashboardAuthenticationAndReadOnly(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	s := NewServer(&config.Config{Port: 8317}, nil, nil, "", WithRequestLoggerFactory(nil))
	for _, test := range []struct {
		method, path, key string
		status            int
	}{
		{http.MethodGet, "/v8/management/account-policy/dashboard", "", 401},
		{http.MethodGet, "/v8/management/account-policy/dashboard", "operator-test", 200},
		{http.MethodGet, "/v8/management/account-policy/dashboard?range=bad", "operator-test", 400},
		{http.MethodPost, "/v8/management/account-policy/dashboard", "operator-test", 404},
		{http.MethodGet, "/v0/management/account-policy/dashboard", "operator-test", 404},
	} {
		r := httptest.NewRequest(test.method, test.path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		if test.key != "" {
			r.Header.Set("Authorization", "Bearer "+test.key)
		}
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s: got %d %s", test.method, test.path, w.Code, w.Body.String())
		}
		if test.status == 200 {
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["sampled_at"] == nil || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing sample timestamp or cache protection")
			}
		}
	}
}
