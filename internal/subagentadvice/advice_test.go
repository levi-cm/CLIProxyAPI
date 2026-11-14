package subagentadvice

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

const (
	opus     = "claude-opus-5-5"
	sol      = "gpt-6.1-sol"
	deepseek = "opencode-go/deepseek-v4.1-flash"
)

// history returns hourly samples ending now with the given pace.
func history(used, pace float64, reset time.Time, hours int) []Sample {
	var out []Sample
	for h := hours; h >= 1; h-- {
		out = append(out, Sample{At: t0.Add(-time.Duration(h) * time.Hour), UsedPercent: used - pace*float64(h), ResetAt: reset})
	}
	return out
}

func claudeAcct(id string, used float64, resetIn time.Duration, pace float64) Account {
	reset := t0.Add(resetIn)
	return Account{
		ID: id, Provider: "claude", Models: []string{opus, "claude-fable-5-2"},
		Windows: []Window{
			{Name: "7d", Long: true, UsedPercent: used, ResetAt: reset, DurationSeconds: 604800, ObservedAt: t0.Add(-time.Minute)},
			{Name: "5h", UsedPercent: 10, ResetAt: t0.Add(2 * time.Hour), DurationSeconds: 18000, ObservedAt: t0.Add(-time.Minute)},
		},
		History: history(used, pace, reset, 6),
	}
}

func codexAcct(id string, used float64, resetIn time.Duration, pace float64) Account {
	reset := t0.Add(resetIn)
	return Account{
		ID: id, Provider: "codex", Plan: "pro", Models: []string{sol, "gpt-6-luna"},
		Windows: []Window{{Name: "7d", Long: true, UsedPercent: used, ResetAt: reset, DurationSeconds: 604800, ObservedAt: t0.Add(-time.Minute)}},
		History: history(used, pace, reset, 6),
	}
}

func opencodeAcct(id string) Account {
	return Account{ID: id, Provider: "opencode-go", Models: []string{deepseek, "opencode-go/glm-5.3"}}
}

var levels = []string{"low", "medium", "high", "xhigh", "max"}

func discovered() []DiscoveredModel {
	return []DiscoveredModel{
		{ID: opus, Provider: "claude", Levels: levels, ContextTokens: 1000000},
		{ID: "claude-fable-5-2", Provider: "claude", Levels: levels, ContextTokens: 1000000},
		{ID: sol, Provider: "codex", Levels: levels, ContextTokens: 272000},
		{ID: deepseek, Provider: "opencode-go"},
	}
}

// normalFleet has only six hours of history: the trend is unknown.
func normalFleet() Inventory {
	return Inventory{
		Accounts: []Account{
			claudeAcct("claude-a", 50, 72*time.Hour, 1),
			claudeAcct("claude-b", 20, 120*time.Hour, 0.5),
			claudeAcct("claude-c", 10, 144*time.Hour, 0.5),
			codexAcct("codex-a", 30, 96*time.Hour, 0.5),
			codexAcct("codex-b", 10, 150*time.Hour, 0.5),
			opencodeAcct("opencode"),
		},
		Models: discovered(),
	}
}

// surplusFleet has an idle Claude account resetting in 10h with 60% unused.
func surplusFleet() Inventory {
	inv := normalFleet()
	inv.Accounts[0] = claudeAcct("claude-a", 40, 10*time.Hour, 0)
	return inv
}

// trendAcct is an account observed now whose weekly use rose by perDay
// percent per day over the last hours.
func trendAcct(a Account, perDay float64, hours int) Account {
	w := &a.Windows[0]
	w.ObservedAt = t0
	a.History = nil
	for h := hours; h >= 1; h-- {
		a.History = append(a.History, Sample{At: t0.Add(-time.Duration(h) * time.Hour), UsedPercent: w.UsedPercent - perDay*float64(h)/24, ResetAt: w.ResetAt})
	}
	return a
}

// trendFleet gives every Claude and Codex account the same daily use over
// the last hours. Budgets: Claude 3 x 100/7 = 42.9%/day, Codex 2 x 100/7 =
// 28.6%/day of one account.
func trendFleet(claudeEach, codexEach float64, hours int) Inventory {
	return Inventory{
		Accounts: []Account{
			trendAcct(claudeAcct("claude-a", 60, 72*time.Hour, 0), claudeEach, hours),
			trendAcct(claudeAcct("claude-b", 55, 120*time.Hour, 0), claudeEach, hours),
			trendAcct(claudeAcct("claude-c", 52, 144*time.Hour, 0), claudeEach, hours),
			trendAcct(codexAcct("codex-a", 60, 96*time.Hour, 0), codexEach, hours),
			trendAcct(codexAcct("codex-b", 55, 150*time.Hour, 0), codexEach, hours),
			opencodeAcct("opencode"),
		},
		Models: discovered(),
	}
}

// eachFor returns the per-account daily use that puts a pool of n accounts
// at ratio of its weekly budget.
func eachFor(ratio float64) float64 { return ratio * 100 / 7 }

type fakeSource struct {
	mu  sync.Mutex
	inv Inventory
	err error
}

func (f *fakeSource) Inventory(context.Context) (Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Inventory{}, f.err
	}
	// Deep enough copy: the forecaster normalizes accounts in place.
	inv := f.inv
	inv.Accounts = append([]Account(nil), f.inv.Accounts...)
	return inv, nil
}

func newTestService(t *testing.T, cfg Config, inv Inventory) (*Service, *fakeSource, *time.Time) {
	t.Helper()
	src := &fakeSource{inv: inv}
	svc := New(cfg, src)
	now := t0
	svc.SetClock(func() time.Time { return now })
	return svc, src, &now
}

func mustAdvise(t *testing.T, svc *Service, req Request) Advice {
	t.Helper()
	advice, err := svc.Advise(context.Background(), req)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if advice.Schema != Schema || advice.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %s/%d", advice.Schema, advice.SchemaVersion)
	}
	return advice
}

func by(a Advice, model string) *ModelAdvice {
	for i := range a.Models {
		if a.Models[i].Model == model {
			return &a.Models[i]
		}
	}
	return nil
}

func posture(a Advice, provider string) *ProviderPosture {
	for i := range a.Postures {
		if a.Postures[i].Provider == provider {
			return &a.Postures[i]
		}
	}
	return nil
}

func TestHarnessHintsResolveToModelNames(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, normalFleet())
	a := mustAdvise(t, svc, Request{})
	o, s := by(a, opus), by(a, sol)
	if o.Harnesses["claude-code"] != "opus" || o.Harnesses["opencode"] != opus || o.Provider != "claude" {
		t.Fatalf("opus harnesses = %+v", o.Harnesses)
	}
	if s.Harnesses["codex"] != sol || s.Harnesses["opencode"] != sol || len(s.Harnesses) != 2 {
		t.Fatalf("sol harnesses = %+v", s.Harnesses)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"claude-a", "codex-a", "acct_"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("advice exposes account identity %q", leaked)
		}
	}
}

func TestFableIsNeverAdvisedByDefault(t *testing.T) {
	cfg := Config{Models: []ModelCapability{
		{Name: "fable", Match: []string{"claude-fable-*"}, Provider: "claude", Normal: Step{Effort: "high"}, Harnesses: map[string]string{"claude-code": "fable"}},
		{Name: "opus", Match: []string{opus}, Provider: "claude", Normal: Step{Effort: "xhigh"}},
	}}
	svc, _, _ := newTestService(t, cfg, surplusFleet())
	a := mustAdvise(t, svc, Request{})
	if strings.Contains(strings.ToLower(a.Text()), "fable\n") || by(a, "claude-fable-5-2") != nil || by(a, opus) == nil {
		t.Fatalf("fable advised or opus missing: %+v", a.Models)
	}
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Models[0].Capability != "fable" || f.Models[0].Reason != "never_advised" {
		t.Fatalf("fable forecast = %+v", f.Models[0])
	}
	// An explicit empty list is the only way to allow it.
	cfg.NeverAdvise = []string{}
	svc, _, _ = newTestService(t, cfg, normalFleet())
	if a = mustAdvise(t, svc, Request{}); by(a, "claude-fable-5-2") == nil {
		t.Fatalf("empty never-advise list should allow fable: %+v", a.Models)
	}
}

func TestAccountRemovalAndAdditionNeedNoRestart(t *testing.T) {
	svc, src, _ := newTestService(t, Config{}, normalFleet())
	src.mu.Lock()
	src.inv.Accounts = []Account{codexAcct("codex-a", 30, 96*time.Hour, 0.5), opencodeAcct("opencode")}
	src.mu.Unlock()
	a := mustAdvise(t, svc, Request{})
	if by(a, opus) != nil || len(a.Models) != 1 || len(a.Warnings) == 0 {
		t.Fatalf("advice after removing claude: %+v %v", a.Models, a.Warnings)
	}
	src.mu.Lock()
	src.inv.Accounts = append(src.inv.Accounts, claudeAcct("claude-new", 5, 100*time.Hour, 0))
	src.mu.Unlock()
	if a = mustAdvise(t, svc, Request{}); by(a, opus) == nil {
		t.Fatalf("new claude account not used: %+v", a.Models)
	}
}

func TestNewProviderThroughConfigurationOnly(t *testing.T) {
	inv := Inventory{
		Accounts: []Account{
			{ID: "g1", Provider: "gemini", Models: []string{"gemini-4-pro"}},
			{ID: "k1", Provider: "kimi", Models: []string{"kimi-k3"}},
		},
		Models: []DiscoveredModel{{ID: "gemini-4-pro", Provider: "gemini", Levels: []string{"low", "high"}}, {ID: "kimi-k3", Provider: "kimi"}},
	}
	cfg := Config{Models: []ModelCapability{{Name: "gemini-pro", Match: []string{"gemini-4-pro*"}, Provider: "gemini", Normal: Step{Effort: "high"}, UseMore: Step{Effort: "xhigh"}, Harnesses: map[string]string{"gemini-cli": ""}}}}
	svc, _, _ := newTestService(t, cfg, inv)
	a := mustAdvise(t, svc, Request{})
	if len(a.Models) != 1 {
		t.Fatalf("models = %+v", a.Models)
	}
	// xhigh is configured but the registry does not advertise it: never selected.
	if m := a.Models[0]; m.Model != "gemini-4-pro" || m.ReasoningEffort != "high" || m.CapabilitySource != "registry+configured" || m.Harnesses["gemini-cli"] != "gemini-4-pro" {
		t.Fatalf("gemini = %+v", m)
	}
}

func TestMissingCapabilityIsNeverSelected(t *testing.T) {
	inv := normalFleet()
	inv.Models[2].Levels = []string{"low"}
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if by(a, sol) != nil || by(a, opus) == nil {
		t.Fatalf("gpt-6.1-sol advised without a supported effort: %+v", a.Models)
	}
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range f.Models {
		if m.Capability == "gpt-6.1-sol" && (m.State != OfferUnavailable || m.Reason != "no_supported_effort") {
			t.Fatalf("forecast model = %+v", m)
		}
	}
}

func TestContextFitOnlyWhenRequested(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, normalFleet())
	if a := mustAdvise(t, svc, Request{ContextTokens: 250000}); by(a, sol) != nil {
		t.Fatalf("model with a 272k context advised for 250k prompts + 32k output")
	}
	if a := mustAdvise(t, svc, Request{}); by(a, sol) == nil {
		t.Fatalf("without context_tokens the context check must not apply: %+v", a.Models)
	}
}

func TestProviderOutageFallsBackToOtherProviders(t *testing.T) {
	inv := normalFleet()
	for i := 3; i < 5; i++ {
		inv.Accounts[i].Blocked, inv.Accounts[i].BlockedUntil = "rate_limited", t0.Add(30*time.Minute)
	}
	inv.Accounts[0].Blocked = "auth_error"
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if by(a, sol) != nil || by(a, opus) == nil || posture(a, "codex").Posture != PostureAvoid {
		t.Fatalf("fallback advice = %+v", a.Models)
	}
}

func TestQuotaResetRollsWindowToUnknown(t *testing.T) {
	inv := normalFleet()
	inv.Accounts[0].Windows[0].ResetAt = t0.Add(-time.Minute)
	svc, _, _ := newTestService(t, Config{}, inv)
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Accounts {
		if a.AccountRef == accountRef("claude", "claude-a") && (a.Metered || a.MeterNote != "window_rolled_over") {
			t.Fatalf("rolled window = %+v", a)
		}
	}
}

func TestFableScopedWindowBlocksOnlyFable(t *testing.T) {
	inv := normalFleet()
	inv.Accounts[0].Windows = append(inv.Accounts[0].Windows, Window{Name: "7d_oi", Long: true, Model: "claude-fable-*", Full: true, ResetAt: t0.Add(time.Hour), ObservedAt: t0})
	svc, _, _ := newTestService(t, Config{}, inv)
	f := newForecaster(svc.Config(), t0, inv, nil)
	st := f.byID["claude-a"]
	if f.eligible(st, "claude-fable-5-2").ok || !f.eligible(st, opus).ok {
		t.Fatalf("fable window must block fable only")
	}
}

func TestMixedCapacityPoolUsesOneUnit(t *testing.T) {
	inv := normalFleet()
	inv.Accounts[4].Plan = "plus"
	cfg := Config{Providers: map[string]ProviderConfig{"codex": {PlanCapacityUnits: map[string]float64{"pro": 5000}}}}
	svc, _, _ := newTestService(t, cfg, inv)
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Accounts {
		if a.AccountRef == accountRef("codex", "codex-b") && (a.CapacityUnits != 5000 || a.CapacityBasis != "pool_minimum") {
			t.Fatalf("unknown capacity in a priced pool = %+v", a)
		}
	}
}

func TestInvalidObservationIsUnmetered(t *testing.T) {
	inv := normalFleet()
	inv.Accounts[0].Windows[0].UsedPercent = math.NaN()
	svc, _, _ := newTestService(t, Config{}, inv)
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Accounts {
		if a.AccountRef == accountRef("claude", "claude-a") && (a.Metered || a.MeterNote != "invalid_observation") {
			t.Fatalf("NaN observation = %+v", a)
		}
	}
}

func TestSamplesAreSpacedAndBounded(t *testing.T) {
	svc, src, now := newTestService(t, Config{}, normalFleet())
	for i := 0; i < 2000; i++ {
		*now = t0.Add(time.Duration(i) * time.Minute)
		src.mu.Lock()
		src.inv.Accounts[0].Windows[0].ObservedAt = *now
		src.mu.Unlock()
		if err := svc.Sample(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(svc.samples["claude-a"]); n > maxSamplesPerAcct || n < 300 {
		t.Fatalf("samples = %d", n)
	}
}

func TestAdviceIsReadOnly(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, surplusFleet())
	first, _ := json.Marshal(mustAdvise(t, svc, Request{}))
	for i := 0; i < 20; i++ {
		if again, _ := json.Marshal(mustAdvise(t, svc, Request{})); string(again) != string(first) {
			t.Fatalf("advice changed after repeated reads")
		}
	}
}

func TestInvalidRequestsAndFailuresAreNotGuessed(t *testing.T) {
	svc, src, _ := newTestService(t, Config{}, normalFleet())
	if _, err := svc.Advise(context.Background(), Request{ContextTokens: -5}); !hasCode(err, "invalid_request") {
		t.Fatalf("err = %v", err)
	}
	src.err = errors.New("boom")
	if _, err := svc.Advise(context.Background(), Request{}); !hasCode(err, "inventory_unavailable") {
		t.Fatalf("err = %v", err)
	}
	disabled := false
	svc.SetConfig(Config{Enabled: &disabled})
	if _, err := svc.Advise(context.Background(), Request{}); !hasCode(err, "subagent_advisor_disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeterministicAdvice(t *testing.T) {
	a, _, _ := newTestService(t, Config{}, surplusFleet())
	b, _, _ := newTestService(t, Config{}, surplusFleet())
	ja, _ := json.Marshal(mustAdvise(t, a, Request{}))
	jb, _ := json.Marshal(mustAdvise(t, b, Request{}))
	if string(ja) != string(jb) {
		t.Fatalf("non-deterministic:\n%s\n%s", ja, jb)
	}
}

func TestPlanCapacityOverrideUsesPriceEstimates(t *testing.T) {
	cfg := Config{Providers: map[string]ProviderConfig{"codex": {PlanCapacityUnits: map[string]float64{"pro": 500}}}}
	svc, _, _ := newTestService(t, cfg, normalFleet())
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Accounts {
		if a.Provider == "codex" && (a.CapacityBasis != "plan" || a.CapacityUnits != 500) {
			t.Fatalf("codex capacity = %+v", a)
		}
	}
}

func hasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func TestPrefixedAndAliasedFableIsNeverAdvised(t *testing.T) {
	acct := claudeAcct("claude-a", 40, 10*time.Hour, 0)
	acct.Models = []string{opus, "best", "team/claude-fable-5-2"}
	acct.Canonical = map[string]string{"best": "claude-fable-5-2"}
	cfg := Config{Models: []ModelCapability{
		{Name: "prefixed", Match: []string{"team/claude-fable-*"}, Provider: "claude", Normal: Step{Effort: "high"}},
		{Name: "alias", Match: []string{"best"}, Provider: "claude", Normal: Step{Effort: "high"}},
		{Name: "opus", Match: []string{opus}, Provider: "claude", Normal: Step{Effort: "xhigh"}},
	}}
	svc, _, _ := newTestService(t, cfg, Inventory{Accounts: []Account{acct}, Models: discovered()})
	a := mustAdvise(t, svc, Request{})
	if len(a.Models) != 1 || a.Models[0].Model != opus {
		t.Fatalf("fable advised through a prefix or alias: %+v", a.Models)
	}
}

func TestUnavailableFirstMatchFallsThrough(t *testing.T) {
	acct := Account{ID: "opencode", Provider: "opencode-go", Models: []string{"opencode-go/deepseek-v4.1", deepseek},
		ModelBlocks: map[string]Block{"opencode-go/deepseek-v4.1": {Reason: "model_cooldown", Until: t0.Add(time.Hour)}}}
	srv := trackerServer(t, trackerOK)
	svc, _, _ := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := by(mustAdvise(t, svc, Request{}), deepseek); d == nil {
		t.Fatal("a cooled-down first match hid the healthy alternative")
	}
}

func TestDiscoveredMetadataIsPerProvider(t *testing.T) {
	inv := normalFleet()
	inv.Models = append(inv.Models, DiscoveredModel{ID: sol, Provider: "other", ContextTokens: 1000})
	svc, _, _ := newTestService(t, Config{}, inv)
	if s := by(mustAdvise(t, svc, Request{ContextTokens: 100000}), sol); s == nil {
		t.Fatal("another provider's metadata for the same ID rejected codex")
	}
}

// Requests only read: pace samples are recorded by the background sampler.
func TestAdviceRequestsRecordNothing(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, surplusFleet())
	mustAdvise(t, svc, Request{})
	if _, err := svc.Forecast(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	n := len(svc.samples)
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("a request recorded %d sample series", n)
	}
	if err := svc.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(svc.samples) == 0 {
		t.Fatal("the sampler recorded nothing")
	}
}

func TestSharedAliasRoutingToFableIsNeverAdvised(t *testing.T) {
	// "best" is Opus on a cooled-down account and Fable on a healthy one.
	a := claudeAcct("claude-a", 40, 72*time.Hour, 0)
	a.Models, a.Canonical = []string{"best"}, map[string]string{"best": opus}
	a.Blocked, a.BlockedUntil = "cooldown", t0.Add(time.Hour)
	b := claudeAcct("claude-b", 40, 72*time.Hour, 0)
	b.Models, b.Canonical = []string{"best"}, map[string]string{"best": "claude-fable-5-2"}
	cfg := Config{Models: []ModelCapability{{Name: "best", Match: []string{"best"}, Provider: "claude", Normal: Step{Effort: "high"}}}}
	svc, _, _ := newTestService(t, cfg, Inventory{Accounts: []Account{a, b}, Models: discovered()})
	if adv := mustAdvise(t, svc, Request{}); len(adv.Models) != 0 {
		t.Fatalf("a shared alias that routes to Fable was advised: %+v", adv.Models)
	}
}

func TestMonthlyWindowBindsWhenTighter(t *testing.T) {
	acct := codexAcct("codex-a", 20, 100*time.Hour, 0.5)
	acct.Windows = append(acct.Windows, Window{Name: "30d", Long: true, UsedPercent: 90, ResetAt: t0.Add(20 * 24 * time.Hour), DurationSeconds: 30 * 86400, ObservedAt: t0.Add(-time.Minute)})
	w, ok := bindingLongWindow(acct.Windows)
	if !ok || w.Name != "30d" {
		t.Fatalf("binding window = %+v", w)
	}
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	adv := mustAdvise(t, svc, Request{})
	if len(adv.Providers) != 1 || len(adv.Providers[0].Allowance) != 1 || adv.Providers[0].Allowance[0].Period != "monthly" || adv.Providers[0].Allowance[0].LeftPercent != 10 {
		t.Fatalf("allowance = %+v", adv.Providers)
	}
}

// liveCodex mirrors the Codex pool on 2026-11-12: one account with 76% of its
// week left, one used up until tomorrow, ~52k and ~54k credits above the
// 7.5k floors and three full-reset grants each.
func liveCodex(creditsOn bool) Inventory {
	a := codexAcct("codex-a", 24, 129*time.Hour, 0.9)
	b := codexAcct("codex-b", 100, 25*time.Hour, 0)
	grants := []time.Time{t0.Add(18 * 24 * time.Hour), t0.Add(18 * 24 * time.Hour), t0.Add(18 * 24 * time.Hour)}
	a.ResetGrants, b.ResetGrants = grants, grants
	a.Credits = &Credits{Balance: 59855, Spendable: 52105, Floor: 7500, Enabled: creditsOn}
	b.Credits = &Credits{Balance: 62176, Spendable: 54426, Floor: 7500, Enabled: creditsOn}
	if creditsOn {
		b.ServingOnCredits = true
	} else {
		b.Blocked, b.BlockedUntil = "allowance_exhausted", t0.Add(25*time.Hour)
	}
	return Inventory{Accounts: []Account{a, b}, Models: discovered()}
}

// liveClaude mirrors the Claude pool on 2026-11-14: C1 nearly used up until its
// reset in 91h, C2 with 92% left for 128h, C3 untouched for 140h; the fleet
// burns about 0.9% of one account per hour, all on C2.
func liveClaude() Inventory {
	return Inventory{Accounts: []Account{
		claudeAcct("claude-a", 99, 91*time.Hour, 0),
		claudeAcct("claude-b", 8, 128*time.Hour, 0.9),
		claudeAcct("claude-c", 0, 140*time.Hour, 0),
	}, Models: discovered()}
}

func TestStaleAndExpiredObservations(t *testing.T) {
	inv := liveClaude()
	inv.Accounts[0].Windows[0].ObservedAt = t0.Add(-30 * time.Minute)
	svc, src, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if a.Status != StatusDegraded || a.Freshness.StaleAccounts != 1 {
		t.Fatalf("stale advice status %s freshness %+v", a.Status, a.Freshness)
	}
	// Expired evidence is never used as quota.
	src.mu.Lock()
	for i := 0; i < 3; i++ {
		src.inv.Accounts[i].Windows[0].ObservedAt = t0.Add(-7 * time.Hour)
	}
	src.mu.Unlock()
	a = mustAdvise(t, svc, Request{})
	if o := by(a, opus); o == nil || o.Posture != PostureNormal || o.ReasoningEffort != "high" || o.Mode != ModeStandard {
		t.Fatalf("expired evidence produced %+v", o)
	}
}

func TestCreditFundedAccountsStayAdvised(t *testing.T) {
	acct := codexAcct("codex-a", 100, 96*time.Hour, 0)
	acct.ServingOnCredits = true
	acct.Windows = append(acct.Windows, Window{Name: "5h", UsedPercent: 100, Full: true, ResetAt: t0.Add(2 * time.Hour), DurationSeconds: 18000, ObservedAt: t0.Add(-time.Minute)})
	acct.Credits = &Credits{Balance: 20000, Spendable: 12500, Floor: 7500, Enabled: true}
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	a := mustAdvise(t, svc, Request{})
	if s := by(a, sol); s == nil || s.Posture == PostureAvoid || a.Postures[0].Trend.CreditsPercent <= 0 {
		t.Fatalf("credit-funded account = %+v %+v", s, a.Postures)
	}
	// Without credits the same full window blocks.
	acct.ServingOnCredits, acct.Credits = false, nil
	svc, _, _ = newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	if s := by(mustAdvise(t, svc, Request{}), sol); s != nil {
		t.Fatalf("full window without credits was advised: %+v", s)
	}
}

func TestExhaustedAccountsAndRetryAfter(t *testing.T) {
	inv := normalFleet()
	for i := 0; i < 3; i++ {
		inv.Accounts[i].Windows[0].UsedPercent = 100
	}
	svc, src, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if by(a, opus) != nil || len(a.Models) != 1 {
		t.Fatalf("exhausted claude advised: %+v", a.Models)
	}
	src.mu.Lock()
	src.inv.Accounts = src.inv.Accounts[:3]
	src.mu.Unlock()
	a = mustAdvise(t, svc, Request{})
	if a.Status != StatusUnavailable || len(a.Postures) != 3 || a.Postures[0].Provider != "claude" || a.Postures[0].Posture != PostureAvoid || a.RetryAfterSeconds == nil || len(a.Models) != 0 {
		t.Fatalf("advice = %s %+v retry %v", a.Status, a.Postures, a.RetryAfterSeconds)
	}
	if want := int((72 * time.Hour).Seconds()); *a.RetryAfterSeconds != want || a.RefreshAfterSeconds != 300 {
		t.Fatalf("retry after %d (want %d), refresh %d %q", *a.RetryAfterSeconds, want, a.RefreshAfterSeconds, a.RefreshReason)
	}
	if !strings.HasPrefix(a.Text(), "claude avoid, codex avoid, opencode-go avoid | reuse until ") || !strings.Contains(a.Text(), "\nclaude avoid: no usable account (allowance_exhausted)\n") {
		t.Fatalf("text = %s", a.Text())
	}
}

func TestReuseWindow(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, liveClaude())
	a := mustAdvise(t, svc, Request{})
	if a.RefreshAfterSeconds != 300 || !a.ValidUntil.Equal(t0.Add(5*time.Minute)) || !a.GeneratedAt.Equal(t0) {
		t.Fatalf("default reuse: %d %q", a.RefreshAfterSeconds, a.RefreshReason)
	}
	svc, _, _ = newTestService(t, Config{ReuseSeconds: 900}, liveClaude())
	if a = mustAdvise(t, svc, Request{}); a.RefreshAfterSeconds != 900 {
		t.Fatalf("configured reuse: %d", a.RefreshAfterSeconds)
	}
	inv := liveClaude()
	inv.Accounts[1].Windows[1].Full = true
	inv.Accounts[1].Windows[1].ResetAt = t0.Add(3 * time.Minute)
	svc, _, _ = newTestService(t, Config{}, inv)
	if a = mustAdvise(t, svc, Request{}); a.RefreshAfterSeconds != 180 || a.RefreshReason != "a 5h window reopens" {
		t.Fatalf("event: %d %q", a.RefreshAfterSeconds, a.RefreshReason)
	}
	inv = liveClaude()
	for i := range inv.Accounts {
		inv.Accounts[i].Blocked, inv.Accounts[i].BlockedUntil = "rate_limited", t0.Add(5*time.Second)
	}
	svc, _, _ = newTestService(t, Config{}, inv)
	if a = mustAdvise(t, svc, Request{}); a.RefreshAfterSeconds != 120 || *a.RetryAfterSeconds != 5 {
		t.Fatalf("minimum: %d retry %v", a.RefreshAfterSeconds, a.RetryAfterSeconds)
	}
}

func TestAdviceReservesCountsAndCapsNothing(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, trendFleet(4, 4, 72))
	a := mustAdvise(t, svc, Request{})
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_parallel", "max_subagents", "lease", "reservation", "budget_tasks", "room_agents", "fleet", "agents", "in_flight"} {
		if strings.Contains(string(raw), "\""+field+"\"") {
			t.Fatalf("advice carries %q: %s", field, raw)
		}
	}
	text := strings.ToLower(a.Text())
	for _, phrase := range []string{"room for", "working", "agents", "spend freely", "devices"} {
		if strings.Contains(text, phrase) {
			t.Fatalf("text carries %q:\n%s", phrase, text)
		}
	}
	for _, leaked := range []string{"claude-a", "codex-a"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("advice exposes identity %q", leaked)
		}
	}
}

func trackerServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const trackerLow = `{"opencode":{"windows":[
 {"window":"5h","left_pct":80,"reset_h":2.5,"reset_in":"2h30m","pace_pct_h":3,"empty_in":"26h","verdict":"on_track","data":"fresh"},
 {"window":"weekly","left_pct":"55","reset_h":"60","pace_pct_h":0.5,"verdict":"on_track","data":"fresh"},
 {"window":"monthly","left_pct":2,"reset_h":216,"reset_in":"9d","pace_pct_h":1.1,"empty_in":"2h","verdict":"too_fast","data":"fresh"}]}}`

const trackerOK = `{"opencode":{"windows":[
 {"window":"monthly","left_pct":40,"reset_h":216,"pace_pct_h":0.1,"verdict":"on_track","data":"fresh"},
 {"window":"5h","left_pct":90,"reset_h":1,"pace_pct_h":1,"verdict":"on_track","data":"stale"}]}}`

// The live tracker on 2026-11-14: 5h full, weekly 24% left, monthly 2% left
// and burning too fast.
const trackerLive = `{"opencode":{"windows":[
 {"window":"5h","left_pct":100,"reset_h":4.1,"verdict":"burst_limit","data":"fresh"},
 {"window":"weekly","left_pct":24,"reset_h":25,"verdict":"too_slow","data":"fresh"},
 {"window":"monthly","left_pct":2,"reset_h":205.6,"verdict":"too_fast","data":"fresh"}]}}`

func TestOpenCodeGoAvoidedWhenTrackerSaysSo(t *testing.T) {
	for name, tc := range map[string]struct{ body, why string }{
		"low":  {trackerLow, "monthly 2% left, running out too fast until Nov 21 12:00 UTC"},
		"live": {trackerLive, "monthly 2% left, running out too fast until Nov 21 01:36 UTC"},
		// One window burning too fast is enough, even with plenty left.
		"too fast": {`{"opencode":{"windows":[{"window":"weekly","left_pct":60,"reset_h":30,"verdict":"too_fast","data":"fresh"},{"window":"5h","left_pct":3,"reset_h":1,"data":"stale"}]}}`, "5h 3% left until Nov 12 13:00 UTC"},
		"blocked":  {`{"opencode":{"windows":[{"window":"5h","left_pct":40,"reset_h":2,"verdict":"blocked","data":"fresh"}]}}`, "5h 40% left, blocked until Nov 12 14:00 UTC"},
	} {
		srv := trackerServer(t, tc.body)
		svc, _, _ := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
		if err := svc.SampleOpenCode(context.Background()); err != nil {
			t.Fatal(err)
		}
		a := mustAdvise(t, svc, Request{})
		d, p := by(a, deepseek), posture(a, OpenCodeProvider)
		reason := tc.why + "; only when Claude and Codex can't serve"
		// Kept as a last resort: listed last, marked avoid, never usable.
		if d == nil || d.Posture != PostureAvoid || p.Posture != PostureAvoid || p.Reason != reason || a.Models[len(a.Models)-1].Model != deepseek {
			t.Fatalf("%s: deepseek %+v posture %+v", name, d, p)
		}
		text := a.Text()
		if !strings.Contains(text, "\nopencode-go avoid: "+reason+"\n") || !strings.Contains(text, "\nopencode-go/deepseek-v4.1-flash effort=high mode=standard | avoid, high | opencode: ") || strings.Contains(text, "budget unknown") || !strings.Contains(strings.SplitN(text, "\n", 2)[0], "opencode-go avoid |") {
			t.Fatalf("%s: text = %s", name, text)
		}
	}
	srv := trackerServer(t, trackerLive)
	svc, _, _ := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := mustAdvise(t, svc, Request{})
	if !strings.Contains(a.Text(), "opencode-go quota: 5h 100% left, resets in 4h; weekly 24% left, resets in 25h; monthly 2% left, resets in 8d13h\n") {
		t.Fatalf("quota line: %s", a.Text())
	}
	var oc *OpenCodeUsage
	for _, p := range a.Providers {
		if p.Provider == OpenCodeProvider {
			oc = p.OpenCode
		}
	}
	if oc == nil || !oc.Reachable || oc.AvoidUntil == nil || len(oc.Windows) != 3 || oc.Windows[2].Window != "monthly" {
		t.Fatalf("opencode = %+v", oc)
	}
	// Only OpenCode Go left: nothing is usable.
	svc, _, _ = newTestService(t, Config{OpenCodeUsageURL: srv.URL}, Inventory{Accounts: []Account{opencodeAcct("opencode")}, Models: discovered()})
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := mustAdvise(t, svc, Request{}); a.Status != StatusUnavailable {
		t.Fatalf("status = %s", a.Status)
	}
}

// A failed or hung tracker fetch is never cached: the next fetch replaces it.
func TestOpenCodeTrackerRecoversAfterFailureAndHang(t *testing.T) {
	var mu sync.Mutex
	status, hang := http.StatusForbidden, make(chan struct{})
	hanging := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		code, h := status, hanging
		mu.Unlock()
		if h {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(trackerOK))
	}))
	t.Cleanup(func() { close(hang); srv.Close() })
	svc, _, now := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err == nil {
		t.Fatal("403 accepted")
	}
	if by(mustAdvise(t, svc, Request{}), deepseek) != nil {
		t.Fatal("advised after a 403")
	}
	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	*now = t0.Add(5 * time.Minute)
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := by(mustAdvise(t, svc, Request{}), deepseek); d == nil || d.Posture != PostureNormal {
		t.Fatalf("the 403 stuck: %+v", d)
	}
	// A hung fetch blocks further fetches only for one reading lifetime.
	mu.Lock()
	hanging = true
	mu.Unlock()
	*now = t0.Add(10 * time.Minute)
	go func() { _ = svc.SampleOpenCode(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.mu.Lock()
		started := !svc.fetchStarted.IsZero()
		svc.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fetch never started")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	hanging = false
	mu.Unlock()
	*now = t0.Add(15 * time.Minute)
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err) // skipped while the hung fetch is young
	}
	*now = t0.Add(26 * time.Minute)
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := by(mustAdvise(t, svc, Request{}), deepseek); d == nil || d.Posture != PostureNormal {
		t.Fatalf("a hung fetch froze the tracker: %+v", d)
	}
}

func TestOpenCodeGoUsableShowsWindows(t *testing.T) {
	srv := trackerServer(t, trackerOK)
	svc, _, now := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := mustAdvise(t, svc, Request{})
	d := by(a, deepseek)
	if d == nil || d.Posture != PostureNormal || d.ReasoningEffort != "high" {
		t.Fatalf("deepseek = %+v", d)
	}
	text := a.Text()
	if !strings.Contains(text, "opencode-go quota: 5h 90% left, resets in 1h (stale); monthly 40% left, resets in 9d\n") || !strings.Contains(text, "opencode-go/deepseek-v4.1-flash effort=high mode=standard | normal, high | opencode: opencode-go/deepseek-v4.1-flash\n") {
		t.Fatalf("text = %s", text)
	}
	// A reading older than 15 minutes counts as unreachable: avoid, budget unknown.
	*now = t0.Add(20 * time.Minute)
	a = mustAdvise(t, svc, Request{})
	if by(a, deepseek) != nil || !strings.Contains(a.Text(), "opencode-go avoid: budget unknown (usage tracker unreachable)\n") || !strings.Contains(strings.SplitN(a.Text(), "\n", 2)[0], "opencode-go avoid |") {
		t.Fatalf("stale tracker text = %s", a.Text())
	}
}

func TestOpenCodeGoTrackerFailures(t *testing.T) {
	for name, body := range map[string]string{"empty": `{}`, "garbage": `nope`, "no-left": `{"opencode":{"windows":[{"window":"5h"}]}}`} {
		srv := trackerServer(t, body)
		svc, _, _ := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
		if err := svc.SampleOpenCode(context.Background()); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if a := mustAdvise(t, svc, Request{}); by(a, deepseek) != nil || !strings.Contains(a.Text(), "opencode-go avoid: budget unknown (usage tracker unreachable)") {
			t.Fatalf("%s: %s", name, a.Text())
		}
	}
	// Without a URL nothing is fetched and the budget stays unknown.
	svc, _, _ := newTestService(t, Config{}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if text := mustAdvise(t, svc, Request{}).Text(); !strings.Contains(text, "opencode-go avoid: budget unknown (no usage tracker configured)") {
		t.Fatalf("unconfigured tracker: %s", text)
	}
}

func TestConfigValidation(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	bad := []Config{
		{SafetyFactor: 0.5},
		{FastMode: "always"},
		{MinRefreshSeconds: 5},
		{MinRefreshSeconds: 600, MaxRefreshSeconds: 300},
		{NeverAdvise: []string{"["}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Normal: Step{Effort: "ultra"}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, UseMore: Step{Effort: "high", Mode: "turbo"}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, UseMore: Step{Effort: "max"}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"["}}}},
		{SafetyFactor: math.NaN()},
		{SurplusHorizonHours: math.Inf(1)},
		{Providers: map[string]ProviderConfig{"claude": {CapacityUnits: math.Inf(1)}}},
		{AccountCapacityUnits: map[string]float64{"a": math.NaN()}},
		{Prices: map[string]Price{"x*": {}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Price: &Price{Input: -1, Output: 2}}}},
		{ReuseSeconds: 10},
		{TrendDays: 0.5},
		{TrendDays: 40},
		{TrendDays: math.NaN()},
		{CreditSpendDays: -1},
		{XHighBelowRatio: 1.5},
		{XHighBelowRatio: -0.1},
		{ConserveAboveRatio: math.Inf(1)},
		{FastHorizonHours: -1},
		{FastMinExpiringPercent: math.Inf(1)},
		{OpenCodeUsageURL: "ftp://x"},
	}
	for i, cfg := range bad {
		if cfg.Validate() == nil {
			t.Fatalf("config %d accepted: %+v", i, cfg)
		}
	}
	r := Config{Providers: map[string]ProviderConfig{"codex": {PlanCapacityUnits: map[string]float64{"Pro": 500}}}}.Resolve()
	if r.Providers["codex"].PlanCapacityUnits["pro"] != 500 || r.Providers["claude"].CapacityUnits != 2000 {
		t.Fatalf("resolve merge = %+v", r.Providers)
	}
	d := DefaultConfig().Resolve()
	if len(d.NeverAdvise) != 1 || d.NeverAdvise[0] != "claude-fable-*" || d.ReuseSeconds != 300 || d.OpenCodeUsageURL != "" ||
		d.TrendDays != 3 || d.CreditSpendDays != 7 || d.XHighBelowRatio != 0.5 || d.ConserveAboveRatio != 1 || d.FastHorizonHours != 24 || d.FastMinExpiringPercent != 50 {
		t.Fatalf("defaults = %+v", d)
	}
	steps := map[string]string{}
	for _, m := range d.Models {
		var parts []string
		for _, s := range m.steps() {
			parts = append(parts, s.Effort+"/"+s.Mode)
		}
		steps[m.Name] = strings.Join(parts, " ")
		if matchModel(d.NeverAdvise, m.Match[0]) {
			t.Fatalf("default model %s is on the never-advise list", m.Name)
		}
	}
	// conserve, normal, use more, fast
	if steps["claude-opus-5.5"] != "high/standard high/standard xhigh/standard xhigh/fast" ||
		steps["gpt-6.1-sol"] != "medium/standard high/standard xhigh/standard xhigh/priority" ||
		steps["deepseek-4.1"] != "high/standard high/standard high/standard high/standard" {
		t.Fatalf("steps = %v", steps)
	}
	// A rule that names only its normal step uses it everywhere.
	one := Config{Models: []ModelCapability{{Name: "o", Match: []string{opus}, Provider: "claude", Normal: Step{Effort: "extra-high"}}}}.Resolve().Models[0]
	if one.Conserve != (Step{Effort: "xhigh", Mode: ModeStandard}) || one.Fast != one.Normal {
		t.Fatalf("single-step rule = %+v", one)
	}
}
