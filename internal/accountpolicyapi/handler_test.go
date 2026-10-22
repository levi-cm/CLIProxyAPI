package accountpolicyapi

import (
	"context"
	"encoding/csv"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyledger"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUnavailableAndStrictMutationInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	h.Register(r.Group("/v1/account-policy"), true)
	for _, path := range []string{"/accounts", "/routing-override", "/usage-history/requests"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/account-policy"+path, nil))
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("%s %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestHistoricalAPIRecordsSummariesExportsAndPrivacy(t *testing.T) {
	a, err := accountpolicyledger.New(t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "codex", AuthID: "fixture", Model: "executed", Alias: "requested", ResponseModel: "served", APIKey: "PRIVATE-KEY", Source: "PRIVATE-SOURCE", RequestedAt: at, Detail: usage.Detail{InputTokens: 1000, CacheReadTokens: 80, TotalTokens: 1000, Measurements: usage.MeasurementPresence{Input: true, CacheRead: true}}})
	}
	if err = a.Observe(accountpolicyledger.Observation{ID: "quota", CredentialID: "fixture", IdentityRef: "PRIVATE-IDENTITY", ObservedAt: at, DurationSeconds: 604800, RemainingPercent: 80}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{History: func() *accountpolicyledger.Archive { return a }}
	r := gin.New()
	h.Register(r.Group("/v1/account-policy"), true)
	for _, path := range []string{"/usage-history/requests?model=served", "/usage-history/summary?group=year&from=2025-01-01T00:00:00Z&to=2027-01-01T00:00:00Z", "/usage-history/export?format=ndjson", "/usage-history/export?format=csv", "/allowance-history?credential_id=fixture&window=weekly&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/account-policy"+path, nil))
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, "PRIVATE-") {
			t.Fatalf("history contract %s %d %s", path, w.Code, body)
		}
		if strings.Contains(path, "summary") && !strings.Contains(body, `"cached_input_percent":8`) {
			t.Fatalf("summary cache coverage %s", body)
		}
		if strings.Contains(path, "format=csv") {
			rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
			if err != nil || len(rows) != 3 || !strings.Contains(body, "requested_model,execution_model,response_model") {
				t.Fatalf("CSV contract %v %v", rows, err)
			}
		}
		if strings.Contains(path, "format=ndjson") && len(strings.Split(strings.TrimSpace(body), "\n")) != 2 {
			t.Fatalf("NDJSON records %s", body)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/account-policy/usage-history/requests?limit=501", nil))
	if w.Code != 400 {
		t.Fatalf("page cap %d", w.Code)
	}
}
