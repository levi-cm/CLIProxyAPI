package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

// creditFixtureProvider reports an exhausted weekly allowance with a credit balance.
type creditFixtureProvider struct{}

func (creditFixtureProvider) Discover(_ context.Context, id accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	blocked, has := false, true
	balance := 62454.42
	return accountpolicy.Snapshot{
		Identity: id, Plan: "pro", Status: "quota_blocked", ObservedAt: at, InventoryObservedAt: at,
		Buckets:       []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 100, Allowed: &blocked, ResetAt: at.Add(96 * time.Hour), ObservedAt: at}},
		CreditBalance: &balance, HasCredits: &has,
	}, nil
}
func (creditFixtureProvider) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	return accountpolicy.ConsumeResult{Code: "nothing_to_reset"}, nil
}

func TestAccountPolicyCreditStatusAndSettings(t *testing.T) {
	r, service := policyTestRouter(t, true, creditFixtureProvider{})
	if err := service.Refresh(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	type response struct {
		Settings     accountpolicy.Settings                `json:"settings"`
		CreditStatus map[string]accountpolicy.CreditStatus `json:"credit_status"`
	}
	read := func() response {
		w := policyRequest(r, http.MethodGet, "/accounts", "", "operator-test")
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		var out response
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := read()
	if status := got.CreditStatus["a"]; status.State != accountpolicy.CreditStateDisabled || status.Balance == nil || *status.Balance != 62454.42 || status.Reserved != 7500 {
		t.Fatalf("disabled credit status = %+v", status)
	}

	w := policyRequest(r, http.MethodPatch, "/settings", `{"credit_spending":{"enabled":true,"floor_credits":7200}}`, "operator-test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	got = read()
	if !got.Settings.CreditSpending.Enabled || got.Settings.CreditSpending.FloorCredits != 7200 {
		t.Fatalf("settings = %+v", got.Settings.CreditSpending)
	}
	if status := got.CreditStatus["a"]; status.State != accountpolicy.CreditStateCredits || status.Floor != 7200 {
		t.Fatalf("enabled credit status = %+v", status)
	}

	// A partial update keeps the custom floor instead of resetting it to the default.
	if w := policyRequest(r, http.MethodPatch, "/settings", `{"credit_spending":{"margin_credits":300}}`, "operator-test"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if c := service.Settings().CreditSpending; c.FloorCredits != 7200 || c.MarginCredits != 300 || !c.Enabled {
		t.Fatalf("partial credit patch lost fields: %+v", c)
	}
	// Unlocks are capped at 24 hours.
	if w := policyRequest(r, http.MethodPatch, "/settings", `{"accounts":{"a":{"credit_reserve_unlocked_until":"2026-10-06T12:00:00Z"}}}`, "operator-test"); w.Code != http.StatusBadRequest {
		t.Fatal("unbounded unlock accepted", w.Code, w.Body.String())
	}
	// Unlock merges into existing account controls without dropping them.
	if w := policyRequest(r, http.MethodPatch, "/settings", `{"accounts":{"a":{"hold":false,"reserve_percent":5}}}`, "operator-test"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	until := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if w := policyRequest(r, http.MethodPatch, "/settings", `{"accounts":{"a":{"credit_reserve_unlocked_until":"`+until+`"}}}`, "operator-test"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	control := service.Settings().Accounts["a"]
	if control.ReservePercent != 5 || control.CreditReserveUnlockedUntil == nil || control.CreditReserveUnlockedUntil.Format(time.RFC3339) != until {
		t.Fatalf("control = %+v", control)
	}

	for _, body := range []string{
		`{"credit_spending":{"floor_credits":-1}}`,
		`{"credit_spending":{"max_balance_age_seconds":7200}}`,
		`{"credit_spending":{"unknown":1}}`,
		`{"credit_spending":{"floor_credits":null}}`,
		`{"credit_spending":[]}`,
	} {
		if w := policyRequest(r, http.MethodPatch, "/settings", body, "operator-test"); w.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d %s", body, w.Code, w.Body.String())
		}
	}
	if service.Settings().CreditSpending.FloorCredits != 7200 {
		t.Fatal("rejected patch mutated settings")
	}
}
