package management

import (
	"net/http"
	"strings"
	"testing"
)

func TestAccountPolicyAutomaticRedemptionAPILock(t *testing.T) {
	r, service := policyTestRouter(t, true)
	w := policyRequest(r, http.MethodPatch, "/settings", `{"automation":"auto_expiring"}`, "operator-test")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{"automation":"off"}`, `{"automation":"notify"}`, `{"enabled":false}`,
		`{"read_only":true}`, `{"automation_required":false}`, `{"credit_types":[]}`, `{"expiry_guard_seconds":0}`,
	} {
		w = policyRequest(r, http.MethodPatch, "/settings", body, "operator-test")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"automation_required"`) {
			t.Fatalf("disable attempt %s returned %d: %s", body, w.Code, w.Body.String())
		}
		if s := service.Settings(); !s.Enabled || s.Automation != "auto_expiring" || s.ReadOnly {
			t.Fatal("live automation disabled")
		}
	}
}
