package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyclient"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Unlike upstream's anonymous inference mode, quota reports must fail closed.
func TestAccountPolicyClientRoutesRequirePrincipalAndRemainReadOnly(t *testing.T) {
	s := NewServer(&config.Config{}, nil, nil, "", WithRequestLoggerFactory(nil))
	for _, path := range []string{"/v1/account-policy/usage?session_id=test", "/v1/account-policy/mcp"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "mcp") {
			method = http.MethodPost
		}
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatalf("anonymous %s returned %d", path, w.Code)
		}
	}
	s = NewServer(&config.Config{}, nil, nil, "", WithRequestLoggerFactory(nil), WithMiddleware(func(c *gin.Context) { c.Set("userApiKey", "caller-test") }))
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/v1/account-policy/usage?session_id=test", 503},
		{http.MethodPost, "/v1/account-policy/usage", 404},
		{http.MethodDelete, "/v1/account-policy/mcp", 404},
	} {
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status {
			t.Fatalf("%s %s: %d", test.method, test.path, w.Code)
		}
	}
}

type clientQuotaFixture struct{ reads, writes int }

func (p *clientQuotaFixture) Discover(_ context.Context, id accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	p.reads++
	now := time.Now().UTC()
	return accountpolicy.Snapshot{Identity: id, Status: "healthy", Eligible: true, ObservedAt: now, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 8, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now}}, LastError: ""}, nil
}
func (p *clientQuotaFixture) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	p.writes++
	return accountpolicy.ConsumeResult{}, nil
}

// Reports must use actual scoped completion evidence, not routing advice or the
// most recently selected account globally. Serving tools must never poll/reset.
func TestAccountPolicyClientReportUsesActualAccountAndNeverPolls(t *testing.T) {
	m := auth.NewManager(nil, nil, nil)
	fixture := &policyTransportFixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fixture.serve(t, w, r) }))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.Codex.DisableCodexCloaking = true
	m.SetConfig(cfg)
	m.RegisterExecutor(executor.NewCodexExecutor(cfg))
	a, err := m.Register(context.Background(), &auth.Auth{ID: "fixture-account", Provider: "codex", Attributes: map[string]string{"api_key": "fixture-a", "base_url": upstream.URL}, Metadata: map[string]any{"email": "selected@example.test", "account_id": "fixture-private-account-id", "access_token": "SECRET-FIXTURE-TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5-codex"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	m.RefreshSchedulerEntry(a.ID)
	settings := accountpolicy.DefaultSettings()
	settings.ObservationsEnabled = true
	settings.StateDir = t.TempDir()
	provider := &clientQuotaFixture{}
	id := accountpolicy.Identity{CredentialID: a.ID, AccountID: "fixture-private-account-id", WorkspaceID: "fixture-private-account-id", Provider: "codex", Generation: a.Generation}
	service, err := accountpolicy.NewService(accountpolicy.Options{Settings: settings, Provider: provider, Accounts: func() []accountpolicy.Identity { return []accountpolicy.Identity{id} }})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, m, nil, "", WithRequestLoggerFactory(nil), WithAccountPolicy(service), WithMiddleware(func(c *gin.Context) { c.Set("userApiKey", c.GetHeader("X-Fixture-Caller")) }))
	inference := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":[{"role":"user","content":"fixture"}]}`))
	inference.Header.Set("Session_id", "thread-one")
	inference.Header.Set("X-Fixture-Caller", "caller-a")
	completed := httptest.NewRecorder()
	s.engine.ServeHTTP(completed, inference)
	if completed.Code != 200 {
		t.Fatalf("fixture inference failed: %d %s", completed.Code, completed.Body.String())
	}
	for _, test := range []struct{ caller, thread, state string }{{"caller-a", "thread-one", "idle"}, {"caller-b", "thread-one", "no_selection"}, {"caller-a", "thread-two", "no_selection"}} {
		r := httptest.NewRequest(http.MethodGet, "/v1/account-policy/usage?session_id="+test.thread, nil)
		r.Header.Set("X-Fixture-Caller", test.caller)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		var report accountpolicyclient.Report
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &report) != nil || report.State != test.state {
			t.Fatalf("wrong session report: %d %s", w.Code, w.Body.String())
		}
		if test.state == "idle" {
			account := report.LastSuccessfulAccount
			if account == nil || account.Label != "selected@example.test" || account.Weekly.UsedPercent == nil || *account.Weekly.UsedPercent != 8 || *account.Weekly.RemainingPercent != 92 || account.Weekly.RefreshAt == nil || report.LastCompletedAt == nil {
				t.Fatalf("incorrect selected quota: %s", w.Body.String())
			}
		} else if report.LastSuccessfulAccount != nil {
			t.Fatal("another session's account leaked")
		}
		if strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "fixture-private-account-id") {
			t.Fatal("private credential material leaked")
		}
	}
	if provider.reads != 1 || provider.writes != 0 {
		t.Fatal("usage read polled the provider or redeemed a reset")
	}
}
