package accountpolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func creditTestSettings() Settings {
	s := DefaultSettings()
	s.Enabled = true
	s.CreditSpending = CreditSpending{Enabled: true}
	return s
}

func creditBalance(v float64) *float64 { return &v }

// exhaustedCreditSnapshot mirrors a Pro account whose weekly allowance is used up.
func exhaustedCreditSnapshot(now time.Time, balance float64) Snapshot {
	blocked := false
	return Snapshot{
		Identity:   Identity{CredentialID: "codex-a", AccountID: "acct-a", Provider: "codex"},
		Plan:       "pro",
		Status:     "quota_blocked",
		ObservedAt: now.Add(-30 * time.Second),
		Buckets: []Bucket{{
			Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 100, Allowed: &blocked,
			ResetAt: now.Add(96 * time.Hour), ObservedAt: now.Add(-30 * time.Second),
		}},
		CreditBalance: creditBalance(balance),
	}
}

func readySnapshot(now time.Time, id string, used float64, resetIn time.Duration) Snapshot {
	allowed := true
	return Snapshot{
		Identity:   Identity{CredentialID: id, AccountID: "acct-" + id, Provider: "codex"},
		Plan:       "pro",
		Status:     "ready",
		Eligible:   true,
		ObservedAt: now.Add(-10 * time.Second),
		Buckets: []Bucket{{
			Scope: "ordinary", DurationSeconds: 604800, UsedPercent: used, Allowed: &allowed,
			ResetAt: now.Add(resetIn), ObservedAt: now.Add(-10 * time.Second),
		}},
		CreditBalance: creditBalance(62500),
	}
}

func TestCodexDiscoverParsesCreditBalance(t *testing.T) {
	cases := map[string]struct {
		credits   string
		balance   *float64
		has       *bool
		unlimited bool
	}{
		"decimal string": {`{"has_credits":true,"unlimited":false,"balance":"62454.4227750000"}`, creditBalance(62454.422775), boolPointer(true), false},
		"number":         {`{"has_credits":true,"unlimited":false,"balance":12.5}`, creditBalance(12.5), boolPointer(true), false},
		"null balance":   {`{"has_credits":false,"unlimited":false,"balance":null}`, nil, boolPointer(false), false},
		"garbage":        {`{"has_credits":true,"unlimited":false,"balance":"NaN"}`, nil, boolPointer(true), false},
		"unlimited":      {`{"has_credits":true,"unlimited":true}`, nil, boolPointer(true), true},
		"absent":         {``, nil, nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var usage map[string]json.RawMessage
			if err := json.Unmarshal([]byte(codexFixture(t, "usage")), &usage); err != nil {
				t.Fatal(err)
			}
			delete(usage, "credits")
			if tc.credits != "" {
				usage["credits"] = json.RawMessage(tc.credits)
			}
			body, _ := json.Marshal(usage)
			inventory := codexFixture(t, "credits")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/usage") {
					_, _ = w.Write(body)
					return
				}
				fmt.Fprint(w, inventory)
			}))
			defer server.Close()
			snapshot, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
			if err != nil {
				t.Fatal(err)
			}
			if (snapshot.CreditBalance == nil) != (tc.balance == nil) || snapshot.CreditBalance != nil && *snapshot.CreditBalance != *tc.balance {
				t.Fatalf("balance = %v, want %v", snapshot.CreditBalance, tc.balance)
			}
			if (snapshot.HasCredits == nil) != (tc.has == nil) || snapshot.HasCredits != nil && *snapshot.HasCredits != *tc.has {
				t.Fatalf("has_credits = %v, want %v", snapshot.HasCredits, tc.has)
			}
			if snapshot.CreditsUnlimited != tc.unlimited {
				t.Fatalf("unlimited = %v", snapshot.CreditsUnlimited)
			}
		})
	}
}

func boolPointer(v bool) *bool { return &v }

func TestCreditStateTransitions(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	base := creditTestSettings()
	future := now.Add(2 * time.Hour)
	past := now.Add(-time.Minute)
	cases := []struct {
		name   string
		mutate func(*Snapshot, *Settings)
		want   string
	}{
		{"disabled by default", func(_ *Snapshot, s *Settings) { s.CreditSpending.Enabled = false }, CreditStateDisabled},
		{"spends above floor", nil, CreditStateCredits},
		{"stops at floor plus margin", func(sn *Snapshot, _ *Settings) { sn.CreditBalance = creditBalance(7500 + 250) }, CreditStateFloorReached},
		{"just above floor plus margin", func(sn *Snapshot, _ *Settings) { sn.CreditBalance = creditBalance(7500 + 250.01) }, CreditStateCredits},
		{"custom floor", func(sn *Snapshot, s *Settings) {
			s.CreditSpending.FloorCredits = 20000
			sn.CreditBalance = creditBalance(20100)
		}, CreditStateFloorReached},
		{"stale balance", func(sn *Snapshot, _ *Settings) { sn.ObservedAt = now.Add(-11 * time.Minute) }, CreditStateUnknown},
		{"failed discovery", func(sn *Snapshot, _ *Settings) { sn.LastError = "provider discovery failed" }, CreditStateUnknown},
		{"unknown balance", func(sn *Snapshot, _ *Settings) { sn.CreditBalance = nil }, CreditStateUnknown},
		{"provider says no credits", func(sn *Snapshot, _ *Settings) { sn.HasCredits = boolPointer(false) }, CreditStateFloorReached},
		{"unlimited", func(sn *Snapshot, _ *Settings) { sn.CreditBalance = nil; sn.CreditsUnlimited = true }, CreditStateCredits},
		{"spend control", func(sn *Snapshot, _ *Settings) { sn.Status = "spend_control_blocked" }, CreditStateUnavailable},
		{"authentication failure", func(sn *Snapshot, _ *Settings) { sn.Status = "unknown" }, CreditStateUnavailable},
		{"allowance remains", func(sn *Snapshot, _ *Settings) {
			*sn = readySnapshot(now, "codex-a", 40, 48*time.Hour)
		}, CreditStateAllowance},
		{"model-specific limit is not bridged", func(sn *Snapshot, _ *Settings) {
			allowed, blocked := true, false
			sn.Status = "ready"
			sn.Buckets = []Bucket{
				{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 100, Allowed: &allowed, ResetAt: now.Add(time.Hour), ObservedAt: now},
				{Scope: "codex_model", Model: "gpt-x", DurationSeconds: 300, UsedPercent: 100, Allowed: &blocked, ResetAt: now.Add(time.Hour), ObservedAt: now},
			}
		}, CreditStateAllowance},
		{"unlock lowers floor", func(sn *Snapshot, s *Settings) {
			sn.CreditBalance = creditBalance(1000)
			s.Accounts = map[string]AccountControl{"codex-a": {CreditReserveUnlockedUntil: &future}}
		}, CreditStateCredits},
		{"expired unlock restores floor", func(sn *Snapshot, s *Settings) {
			sn.CreditBalance = creditBalance(1000)
			s.Accounts = map[string]AccountControl{"codex-a": {CreditReserveUnlockedUntil: &past}}
		}, CreditStateFloorReached},
		{"unlock still keeps margin above zero", func(sn *Snapshot, s *Settings) {
			sn.CreditBalance = creditBalance(200)
			s.Accounts = map[string]AccountControl{"codex-a": {CreditReserveUnlockedUntil: &future}}
		}, CreditStateFloorReached},
		{"non-codex provider", func(sn *Snapshot, _ *Settings) { sn.Identity.Provider = "claude" }, CreditStateDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := exhaustedCreditSnapshot(now, 62454.42)
			settings := clone(base)
			if tc.mutate != nil {
				tc.mutate(&snapshot, &settings)
			}
			got := CreditState(snapshot, "gpt-x", settings, now)
			if got.State != tc.want {
				t.Fatalf("state = %s, want %s (%+v)", got.State, tc.want, got)
			}
			eval := Evaluate(snapshot, "gpt-x", settings, now)
			if eval.Credits != (tc.want == CreditStateCredits) {
				t.Fatalf("evaluation credits = %v for state %s", eval.Credits, tc.want)
			}
		})
	}
}

func TestCreditStatusAmounts(t *testing.T) {
	now := time.Now().UTC()
	status := CreditState(exhaustedCreditSnapshot(now, 62454.42), "", creditTestSettings(), now)
	if status.Floor != 7500 || status.Margin != 250 || status.Reserved != 7500 || status.Spendable < 54704.41 || status.Spendable > 54704.43 || !status.Fresh {
		t.Fatalf("unexpected amounts %+v", status)
	}
	low := CreditState(exhaustedCreditSnapshot(now, 3000), "", creditTestSettings(), now)
	if low.Spendable != 0 || low.Reserved != 3000 {
		t.Fatalf("unexpected low amounts %+v", low)
	}
}

func TestEvaluateCreditRankingAndGuard(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	settings := creditTestSettings()
	credit := Evaluate(exhaustedCreditSnapshot(now, 62454.42), "", settings, now)
	if !credit.Eligible || !credit.Known || !credit.Credits || credit.Reason != "credit_balance" || !credit.Deadline.Equal(now.Add(36*time.Hour)) {
		t.Fatalf("unexpected credit evaluation %+v", credit)
	}
	// Allowance renewing after the guard horizon ranks behind credit spending.
	later := Evaluate(readySnapshot(now, "codex-b", 7, 6*24*time.Hour), "", settings, now)
	if !later.Eligible || !credit.Deadline.Before(later.Deadline) {
		t.Fatalf("credit account must rank first: credit=%v allowance=%v", credit.Deadline, later.Deadline)
	}
	// Allowance renewing within the guard horizon would be lost; it ranks first.
	soon := Evaluate(readySnapshot(now, "codex-b", 40, 20*time.Hour), "", settings, now)
	if !soon.Deadline.Before(credit.Deadline) {
		t.Fatalf("renewing allowance must rank first: credit=%v allowance=%v", credit.Deadline, soon.Deadline)
	}
	settings.CreditSpending.AllowanceGuardHours = 12
	shorter := Evaluate(exhaustedCreditSnapshot(now, 62454.42), "", settings, now)
	if !shorter.Deadline.Equal(now.Add(12 * time.Hour)) {
		t.Fatalf("guard horizon not applied: %v", shorter.Deadline)
	}
}

func TestEvaluateCreditSpendingRespectsOperatorControls(t *testing.T) {
	now := time.Now().UTC()
	settings := creditTestSettings()
	settings.Accounts = map[string]AccountControl{"codex-a": {Hold: true}}
	if e := Evaluate(exhaustedCreditSnapshot(now, 62454.42), "", settings, now); e.Eligible || e.Reason != "account_hold" {
		t.Fatalf("hold must win over credits: %+v", e)
	}
	settings = creditTestSettings()
	settings.ForceAccount = "codex-other"
	if e := Evaluate(exhaustedCreditSnapshot(now, 62454.42), "", settings, now); e.Eligible {
		t.Fatalf("forced account must win over credits: %+v", e)
	}
	settings = creditTestSettings()
	settings.Enabled = false
	if e := Evaluate(exhaustedCreditSnapshot(now, 62454.42), "", settings, now); e.Credits || e.Reason != "policy_disabled" {
		t.Fatalf("disabled policy must not report credits: %+v", e)
	}
	settings = creditTestSettings()
	settings.CreditSpending.Enabled = false
	if e := Evaluate(exhaustedCreditSnapshot(now, 62454.42), "", settings, now); e.Eligible {
		t.Fatalf("disabled credit spending must keep exhausted accounts blocked: %+v", e)
	}
}

func TestValidateCreditSpending(t *testing.T) {
	for _, c := range []CreditSpending{
		{FloorCredits: -1}, {MarginCredits: -1}, {MaxBalanceAgeSeconds: -1}, {MaxBalanceAgeSeconds: 3601},
		{AllowanceGuardHours: 169}, {CreditValueUSD: -0.1}, {USDToEUR: 11},
	} {
		s := DefaultSettings()
		s.CreditSpending = c
		if ValidateSettings(s) == nil {
			t.Fatalf("accepted invalid credit spending %+v", c)
		}
	}
	s := DefaultSettings()
	s.CreditSpending = CreditSpending{Enabled: true, FloorCredits: 7500, MarginCredits: 250, MaxBalanceAgeSeconds: 600, AllowanceGuardHours: 36, CreditValueUSD: 0.04, USDToEUR: 0.87}
	if err := ValidateSettings(s); err != nil {
		t.Fatalf("rejected valid credit spending: %v", err)
	}
}

func TestCreditBurnPerHour(t *testing.T) {
	s := &Service{}
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	record := func(at time.Duration, balance float64) {
		s.recordCreditSampleLocked("codex-a", Snapshot{ObservedAt: start.Add(at), CreditBalance: creditBalance(balance)})
	}
	record(0, 10000)
	record(5*time.Minute, 9990)
	if _, ok := s.CreditBurnPerHour("codex-a"); ok {
		t.Fatal("burn rate reported before minimum span")
	}
	record(30*time.Minute, 9900)
	record(60*time.Minute, 9700)
	record(61*time.Minute, 9800) // a top-up is not negative spend
	burn, ok := s.CreditBurnPerHour("codex-a")
	if !ok || burn < 294.5 || burn > 295.5 {
		t.Fatalf("burn = %v ok=%v, want ~295 over 61 minutes", burn, ok)
	}
	record(8*time.Hour, 9800)
	if _, ok := s.CreditBurnPerHour("codex-a"); ok {
		t.Fatal("old samples must be pruned")
	}
	s.recordCreditSampleLocked("codex-a", Snapshot{ObservedAt: start.Add(9 * time.Hour)})
	if len(s.creditSamples["codex-a"]) != 1 {
		t.Fatal("unknown balance must not be sampled")
	}
}

func TestCreditSpendingAdoptsStartupConfigOnlyForNewJournals(t *testing.T) {
	dir := t.TempDir()
	settings := DefaultSettings()
	settings.Enabled = true
	settings.StateDir = dir
	accounts := func() []Identity { return nil }
	first, err := NewService(Options{Settings: settings, Provider: &fakeProvider{}, Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	// Simulate a journal written before credit spending existed.
	path := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	var journalSettings map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(root["settings"], &journalSettings) != nil {
		t.Fatal("unreadable journal")
	}
	delete(journalSettings, "credit_spending")
	root["settings"], _ = json.Marshal(journalSettings)
	data, _ = json.Marshal(root)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	configured := settings
	configured.CreditSpending = CreditSpending{Enabled: true, FloorCredits: 7200}
	second, err := NewService(Options{Settings: configured, Provider: &fakeProvider{}, Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Settings().CreditSpending; !got.Enabled || got.FloorCredits != 7200 {
		t.Fatalf("journal without credit settings did not adopt config: %+v", got)
	}
	operator := second.Settings()
	operator.CreditSpending = CreditSpending{Enabled: false, FloorCredits: 9000}
	if err := second.UpdateSettings(operator); err != nil {
		t.Fatal(err)
	}
	third, err := NewService(Options{Settings: configured, Provider: &fakeProvider{}, Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if got := third.Settings().CreditSpending; got.Enabled || got.FloorCredits != 9000 {
		t.Fatalf("operator credit settings were overwritten by config: %+v", got)
	}
}

func TestCreditSpendingDefersSavedResetUntilFloor(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 4)
	blocked := false
	b.Status, b.Eligible = "quota_blocked", false
	b.Buckets[1].UsedPercent, b.Buckets[1].Allowed = 100, &blocked
	b.Credits = []Credit{testCredit("saved-1", nil), testCredit("saved-2", nil)}
	b.AvailableCredits = 2
	b.CreditBalance = creditBalance(62454.42)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_all_saved"
	settings.CreditSpending = CreditSpending{Enabled: true}
	s, _ := fixtureService(t, p, &now, settings)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("saved reset redeemed while credits serve the account: %v", p.credits)
	}
	p.mu.Lock()
	low := p.snapshots["b"]
	low.CreditBalance = creditBalance(7600)
	p.snapshots["b"] = low
	p.mu.Unlock()
	if err := s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("saved reset not redeemed once the credit floor was reached: %v", p.credits)
	}
}

func TestCreditSpendingDisableSurvivesRestartWithEnabledConfig(t *testing.T) {
	dir := t.TempDir()
	configured := DefaultSettings()
	configured.Enabled = true
	configured.StateDir = dir
	configured.CreditSpending = CreditSpending{Enabled: true}
	accounts := func() []Identity { return nil }
	first, err := NewService(Options{Settings: configured, Provider: &fakeProvider{}, Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	disabled := first.Settings()
	disabled.CreditSpending = CreditSpending{}
	if err := first.UpdateSettings(disabled); err != nil {
		t.Fatal(err)
	}
	second, err := NewService(Options{Settings: configured, Provider: &fakeProvider{}, Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if second.Settings().CreditSpending.Enabled {
		t.Fatal("operator disable was undone by startup configuration")
	}
}

func TestCreditUnlockIsTimeBoxed(t *testing.T) {
	now := testTime()
	p := &fakeProvider{snapshots: map[string]Snapshot{}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	for _, tc := range []struct {
		until time.Time
		ok    bool
	}{{now.Add(4 * time.Hour), true}, {now.Add(MaxCreditUnlock), true}, {now.Add(MaxCreditUnlock + time.Minute), false}, {now.Add(-time.Hour), true}} {
		next := s.Settings()
		until := tc.until
		next.Accounts = map[string]AccountControl{"b": {CreditReserveUnlockedUntil: &until}}
		if err := s.UpdateSettings(next); (err == nil) != tc.ok {
			t.Fatalf("unlock until %v: err=%v, want ok=%v", tc.until, err, tc.ok)
		}
	}
}
