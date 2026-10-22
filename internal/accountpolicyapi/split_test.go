package accountpolicyapi

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
)

type splitProvider struct {
	snapshots map[string]accountpolicy.Snapshot
}

func (p splitProvider) Discover(_ context.Context, id accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	return p.snapshots[id.CredentialID], nil
}
func (p splitProvider) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	return accountpolicy.ConsumeResult{}, nil
}

func TestPercentageSplitAPIAndSingleCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	p := splitProvider{snapshots: map[string]accountpolicy.Snapshot{}}
	ids := []accountpolicy.Identity{}
	for _, id := range []string{"a", "b"} {
		identity := accountpolicy.Identity{CredentialID: id, Provider: "codex", AccountID: "PRIVATE-" + id, WorkspaceID: "PRIVATE-WORKSPACE"}
		ids = append(ids, identity)
		p.snapshots[id] = accountpolicy.Snapshot{Identity: identity, Status: "healthy", Eligible: true, ObservedAt: now, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 10, ObservedAt: now, ResetAt: now.Add(24 * time.Hour)}}}
	}
	settings := accountpolicy.DefaultSettings()
	settings.Enabled = true
	settings.StateDir = t.TempDir()
	service, err := accountpolicy.NewService(accountpolicy.Options{Settings: settings, Provider: p, Accounts: func() []accountpolicy.Identity { return ids }, Now: func() time.Time { return now }})
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
	response := call("PUT", "/routing-override", `{"mode":"split","accounts":[{"credential_id":"a","percent":37},{"credential_id":"b","percent":63}],"duration_hours":2,"expected_revision":0}`)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "PRIVATE-") {
		t.Fatalf("split response: %d %s", response.Code, response.Body.String())
	}
	var result accountpolicy.RoutingOverride
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Mode != "split" || len(result.Accounts) != 2 || result.Accounts[0].Percent != 37 {
		t.Fatalf("split contract: %+v %v", result, err)
	}
	for _, body := range []string{
		`{"mode":"split","accounts":[{"credential_id":"a","percent":0},{"credential_id":"b","percent":100}],"duration_hours":2,"expected_revision":1}`,
		`{"mode":"split","accounts":[{"credential_id":"a","percent":37.5},{"credential_id":"b","percent":62.5}],"duration_hours":2,"expected_revision":1}`,
		`{"mode":"split","credential_id":"a","accounts":[{"credential_id":"a","percent":50},{"credential_id":"b","percent":50}],"duration_hours":2,"expected_revision":1}`,
		`{"mode":"other","credential_id":"a","duration_hours":2,"expected_revision":1}`,
		`{"mode":"single","credential_id":"a","accounts":[{"credential_id":"a","percent":50},{"credential_id":"b","percent":50}],"duration_hours":2,"expected_revision":1}`,
	} {
		w := call("PUT", "/routing-override", body)
		if w.Code != 400 {
			t.Fatalf("invalid body returned %d: %s", w.Code, w.Body.String())
		}
	}
	if w := call("PUT", "/routing-override", `{"credential_id":"a","duration_hours":1,"expected_revision":1}`); w.Code != 200 {
		t.Fatalf("legacy single request: %d %s", w.Code, w.Body.String())
	}
	if w := call("DELETE", "/routing-override?expected_revision=2", ""); w.Code != 200 {
		t.Fatalf("cancel replacement: %d", w.Code)
	}
}
