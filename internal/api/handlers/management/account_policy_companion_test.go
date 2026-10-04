package management

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func companionRouter(t *testing.T, enabled bool) *gin.Engine {
	t.Helper()
	r, service := policyTestRouter(t, enabled)
	h := NewHandler(nil, "", nil)
	h.SetAccountPolicy(service)
	g := r.Group("/v8/management/account-policy", h.AccountPolicyMiddleware())
	g.GET("/capabilities", h.GetAccountPolicyCapabilities)
	g.POST("/resets/redeem-confirmed", h.RedeemAccountPolicyConfirmedReset)
	return r
}

func TestCompanionCapabilitiesAuthenticatedVersionAndAuthority(t *testing.T) {
	r := companionRouter(t, true)
	if got := policyRequest(r, http.MethodGet, "/capabilities", "", ""); got.Code != 401 {
		t.Fatal("capabilities leaked without auth")
	}
	w := policyRequest(r, http.MethodGet, "/capabilities", "", "operator-test")
	var data struct {
		ContractVersion      int  `json:"contract_version"`
		ConfirmationLifetime int  `json:"confirmation_lifetime_seconds"`
		CallerIdempotency    bool `json:"caller_idempotency"`
		SnapshotPrecondition bool `json:"snapshot_precondition"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || data.ContractVersion != 1 || data.ConfirmationLifetime != 120 || !data.CallerIdempotency || !data.SnapshotPrecondition {
		t.Fatal(w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("capability response can be cached")
	}
	if got := policyRequest(companionRouter(t, false), http.MethodGet, "/capabilities", "", "operator-test"); got.Code != 503 {
		t.Fatal("missing native service advertised usable contract")
	}
}

func TestCompanionConfirmedResetRejectsStrictAndUnauthenticatedBodies(t *testing.T) {
	r := companionRouter(t, true)
	if got := policyRequest(r, http.MethodPost, "/resets/redeem-confirmed", `{}`, ""); got.Code != 401 {
		t.Fatal("confirmed reset is unauthenticated")
	}
	for _, body := range []string{`{}`, `{"unknown":true}`, `{} {}`, `{"credential_id":"a","credit_id":"c","request_id":"invalid"}`} {
		w := policyRequest(r, http.MethodPost, "/resets/redeem-confirmed", body, "operator-test")
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
