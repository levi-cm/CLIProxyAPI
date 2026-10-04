package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Copying Auth or its free-form error/status metadata would leak credential secrets.
func TestAccountPolicyDashboardAllProvidersDisabledAndPrivacy(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	m := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "a", Provider: "codex", Label: "Work account", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "secret-token", "email": "private-email"}},
		{ID: "b", Provider: "claude", Label: "Bearer secret-token", Status: coreauth.StatusDisabled, Disabled: true, Metadata: map[string]any{"access_token": "secret-token"}},
		{ID: "c", Provider: "gemini", Label: "safe", Status: coreauth.Status("secret-status"), StatusMessage: "secret-message", ProxyURL: "https://secret-proxy", LastError: &coreauth.Error{Message: "secret-error"}},
		{ID: "d", Provider: "codex", Label: "prefix password-value", Status: coreauth.StatusActive, Metadata: map[string]any{"refresh_token": "password-value"}},
	} {
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(&config.Config{}, "", m)
	r := gin.New()
	r.GET("/v8/management/account-policy/dashboard", h.AccountPolicyMiddleware(), h.GetAccountPolicyDashboard)
	w := policyRequest(r, http.MethodGet, "/dashboard?range=1h", "", "operator-test")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"secret-", "private-email", "password-value", "access_token", "proxy_url", "metadata", "status_message"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("dashboard leaked %s: %s", secret, w.Body.String())
		}
	}
	var body struct {
		Activity struct {
			Available bool `json:"available"`
			Accounts  []struct {
				CredentialID, Alias, Provider, Status string
				Disabled                              bool
				ActiveRequests                        int `json:"active_requests"`
				LastSelectedAt                        any `json:"last_selected_at"`
			} `json:"accounts"`
		} `json:"activity"`
		Usage struct {
			Available, Collecting bool
			RangeSeconds          int64 `json:"range_seconds"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Activity.Available || len(body.Activity.Accounts) != 4 || body.Usage.Available || body.Usage.Collecting || body.Usage.RangeSeconds != 3600 {
		t.Fatalf("wrong availability or account set: %s", w.Body.String())
	}
	if body.Activity.Accounts[0].Alias != "Work account" || body.Activity.Accounts[1].Provider != "claude" || !body.Activity.Accounts[1].Disabled || body.Activity.Accounts[2].Status != "unknown" {
		t.Fatalf("wrong aliases/status: %s", w.Body.String())
	}
	for _, account := range body.Activity.Accounts {
		if account.ActiveRequests != 0 || account.LastSelectedAt != nil {
			t.Fatal("idle account fabricated live selection")
		}
	}
	stored, _ := m.GetByID("d")
	if stored.Label != "prefix password-value" {
		t.Fatal("read mutated auth metadata")
	}
}

// Repeated dashboard requests must preserve usage and follow the current lifecycle sink.
func TestAccountPolicyDashboardLifecycleUsageReadOnly(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "operator-test")
	sink, err := accountpolicyusage.New(t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := sink.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	sink.HandleUsage(context.Background(), usage.Record{AuthID: "a", RequestedAt: time.Now().Add(-time.Minute), Latency: 50 * time.Millisecond, Detail: usage.Detail{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}, APIKey: "secret-key", Source: "secret-source"})
	h := NewHandler(&config.Config{}, "", coreauth.NewManager(nil, nil, nil))
	var current *accountpolicyusage.Sink
	h.SetAccountPolicyUsage(func() *accountpolicyusage.Sink { return current })
	r := gin.New()
	r.GET("/v8/management/account-policy/dashboard", h.AccountPolicyMiddleware(), h.GetAccountPolicyDashboard)
	read := func() accountpolicyusage.DashboardSnapshot {
		w := policyRequest(r, http.MethodGet, "/dashboard", "", "operator-test")
		if w.Code != 200 || strings.Contains(w.Body.String(), "secret-") {
			t.Fatal(w.Code, w.Body.String())
		}
		var body struct {
			Usage accountpolicyusage.DashboardSnapshot `json:"usage"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Usage
	}
	if s := read(); s.Available || s.Collecting {
		t.Fatal("missing lifecycle sink fabricated collection")
	}
	current = sink
	for i := 0; i < 3; i++ {
		if s := read(); !s.Available || !s.Collecting || s.Totals.Requests != 1 || s.Totals.TotalTokens == nil || *s.Totals.TotalTokens != 10 || s.RangeSeconds != 86400 {
			t.Fatalf("read changed retained usage: %+v", s)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if s := read(); !s.Available || s.Collecting || s.Totals.Requests != 1 {
		t.Fatalf("closed sink lost history or kept collecting: %+v", s)
	}
	current = nil
	if s := read(); s.Available || s.Collecting {
		t.Fatal("released lifecycle sink remained available")
	}
}

// A free-form label can contain credential material stored outside token fields.
func TestAccountPolicyDashboardAliasRedactsAuthorizationAndCookies(t *testing.T) {
	for _, auth := range []*coreauth.Auth{
		{Label: "Team secret-authorization", Attributes: map[string]string{"authorization": "secret-authorization"}},
		{Label: "Team secret-cookie", Metadata: map[string]any{"session_cookie": "secret-cookie"}},
		{Label: "Team plainCredential123", Attributes: map[string]string{"header:Authorization": "Bearer plainCredential123"}},
		{Label: "Team plainCookie456", Attributes: map[string]string{"header:Cookie": "session=plainCookie456"}},
	} {
		if got := dashboardAlias(auth); got != "Account" {
			t.Fatalf("credential-derived label exposed: %q", got)
		}
	}
}
