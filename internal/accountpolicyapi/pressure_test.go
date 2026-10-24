package accountpolicyapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

func TestPressureOverrideAPIExpiryCancellationAndRestart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	ids := []accountpolicy.Identity{}
	p := splitProvider{snapshots: map[string]accountpolicy.Snapshot{}}
	for _, id := range []string{"a", "b"} {
		identity := accountpolicy.Identity{CredentialID: id, Provider: "codex", AccountID: "PRIVATE-" + id}
		ids = append(ids, identity)
		p.snapshots[id] = accountpolicy.Snapshot{Identity: identity, Eligible: true, Status: "healthy", ObservedAt: now, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 10, ObservedAt: now, ResetAt: now.Add(120 * time.Hour)}}}
	}
	settings := accountpolicy.DefaultSettings()
	settings.Enabled = true
	settings.StateDir = t.TempDir()
	opts := accountpolicy.Options{Settings: settings, Provider: p, Accounts: func() []accountpolicy.Identity { return ids }, Now: func() time.Time { return now }}
	service, err := accountpolicy.NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	h := &Handler{Service: service}
	h.Register(r.Group("/v1/account-policy"), true)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/v1/account-policy"+path, strings.NewReader(body)))
		return w
	}
	body := `{"mode":"pressure","accounts":[{"credential_id":"a"},{"credential_id":"b"}],"duration_hours":1,"expected_revision":0}`
	response := call("PUT", "/routing-override", body)
	if response.Code != 200 || strings.Contains(response.Body.String(), "PRIVATE-") {
		t.Fatalf("pressure response: %d %s", response.Code, response.Body.String())
	}
	var active accountpolicy.RoutingOverride
	if err := json.Unmarshal(response.Body.Bytes(), &active); err != nil || active.Mode != "pressure" || !active.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("contract: %+v %v", active, err)
	}
	if response := call("PUT", "/routing-override", body); response.Code != 409 {
		t.Fatalf("stale revision: %d", response.Code)
	}
	restarted, err := accountpolicy.NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.Service = restarted
	for _, bad := range []string{
		`{"mode":"pressure","accounts":[{"credential_id":"a","percent":50},{"credential_id":"b","percent":50}],"duration_hours":1,"expected_revision":1}`,
		`{"mode":"pressure","accounts":[{"credential_id":"a"},{"credential_id":"a"}],"duration_hours":1,"expected_revision":1}`,
		`{"mode":"pressure","credential_id":"a","accounts":[{"credential_id":"a"},{"credential_id":"b"}],"duration_hours":1,"expected_revision":1}`,
	} {
		if response := call("PUT", "/routing-override", bad); response.Code != 400 {
			t.Fatalf("invalid pressure: %d", response.Code)
		}
	}
	// Selection-time preference must expire before maintenance/dashboard polling.
	now = now.Add(time.Hour)
	// Exercise public read method without requiring new interfaces at compile time.
	source, ok := any(restarted).(interface {
		RoutingPressure(string) (string, []accountpolicy.RoutingShare)
	})
	if !ok {
		t.Fatal("pressure preference unavailable")
	}
	if id, _ := source.RoutingPressure("codex"); id != "" {
		t.Fatal("expired authority survived selection")
	}
	if err := restarted.MaintainOverride(); err != nil {
		t.Fatal(err)
	}
	if restarted.RoutingOverride().State != "expired" {
		t.Fatal("expiry not persisted")
	}
	if response := call("DELETE", "/routing-override?expected_revision=2", ""); response.Code != 200 {
		t.Fatalf("cancel: %d", response.Code)
	}
}
