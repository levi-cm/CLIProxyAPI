package subagentadvice

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

// normalFleet has no allowance projected to expire within the horizon.
func normalFleet() Inventory {
	return Inventory{
		Accounts: []Account{
			claudeAcct("claude-a", 50, 72*time.Hour, 1),
			claudeAcct("claude-b", 20, 120*time.Hour, 0),
			claudeAcct("claude-c", 10, 144*time.Hour, 0),
			codexAcct("codex-a", 30, 96*time.Hour, 0.5),
			codexAcct("codex-b", 10, 150*time.Hour, 0),
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

func noSurplus(t *testing.T, a Advice, why string) {
	t.Helper()
	for _, m := range a.Models {
		if m.Basis == KindSurplus || m.Mode != ModeStandard {
			t.Fatalf("%s: %+v", why, m)
		}
	}
	if a.Posture == PostureSpendFreely {
		t.Fatalf("%s: posture %s", why, a.Posture)
	}
}

func TestDefaultAdviceListsEveryUsableModel(t *testing.T) {
	svc, src, _ := newTestService(t, Config{}, normalFleet())
	a := mustAdvise(t, svc, Request{})
	if a.Status != StatusReady || a.Posture != PostureNormal || len(a.Models) != 3 {
		t.Fatalf("advice = %s %s %+v", a.Status, a.Posture, a.Models)
	}
	want := [][4]string{{opus, "xhigh", ModeStandard, KindDefault}, {sol, "high", ModeStandard, KindDefault}, {deepseek, "high", ModeStandard, KindUnmetered}}
	for i, w := range want {
		m := a.Models[i]
		if m.Model != w[0] || m.ReasoningEffort != w[1] || m.Mode != w[2] || m.Basis != w[3] {
			t.Fatalf("model %d = %+v, want %v", i, m, w)
		}
	}
	if a.Models[0].BudgetTasks == nil || *a.Models[0].BudgetTasks < 100 || a.Models[0].BudgetUntil == nil {
		t.Fatalf("opus budget = %+v", a.Models[0])
	}
	if a.Models[2].BudgetTasks != nil {
		t.Fatalf("unmetered model must not invent a budget: %+v", a.Models[2])
	}
	src.mu.Lock()
	src.inv.Accounts = src.inv.Accounts[:5]
	src.mu.Unlock()
	if a = mustAdvise(t, svc, Request{}); len(a.Models) != 2 || by(a, deepseek) != nil {
		t.Fatalf("without opencode: %+v", a.Models)
	}
}

func TestAdviceNeverLimitsSubagentCount(t *testing.T) {
	inv := surplusFleet()
	inv.Accounts = append(inv.Accounts, claudeAcct("claude-d", 30, 20*time.Hour, 0), claudeAcct("claude-e", 35, 30*time.Hour, 0))
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_parallel", "count", "max_subagents"} {
		if strings.Contains(string(raw), "\""+field+"\"") {
			t.Fatalf("advice carries a subagent limit %q: %s", field, raw)
		}
	}
	if strings.Contains(a.Text(), "at most") {
		t.Fatalf("text limits subagents: %s", a.Text())
	}
	// More expiring allowance means a larger budget, not a cap.
	small, _, _ := newTestService(t, Config{}, surplusFleet())
	if *by(a, opus).BudgetTasks <= *by(mustAdvise(t, small, Request{}), opus).BudgetTasks {
		t.Fatalf("budget did not grow with surplus accounts")
	}
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
		{Name: "fable", Match: []string{"claude-fable-*"}, Provider: "claude", Efforts: []string{"high"}, Harnesses: map[string]string{"claude-code": "fable"}},
		{Name: "opus", Match: []string{opus}, Provider: "claude", Efforts: []string{"xhigh"}},
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

func TestSurplusSpendsFreelyButNeverMaxForOpus(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, surplusFleet())
	a := mustAdvise(t, svc, Request{})
	o := by(a, opus)
	if a.Posture != PostureSpendFreely || a.Status != StatusReady || a.Models[0].Model != opus {
		t.Fatalf("advice = %s %s %+v", a.Posture, a.Status, a.Models)
	}
	if o.ReasoningEffort != "xhigh" || o.Mode != ModeFast || o.Basis != KindSurplus {
		t.Fatalf("opus = %+v", o)
	}
	// 60% of 13000 units at 0.6 pace confidence is 4680 units; a fast xhigh
	// task costs (300k*$4 + 40k*1.5*$20)/1e6 * 2 = 4.8 units: 975 tasks.
	if o.BudgetTasks == nil || *o.BudgetTasks != 975 || o.BudgetUntil == nil || !o.BudgetUntil.Equal(t0.Add(10*time.Hour)) {
		t.Fatalf("opus budget = %v until %v", o.BudgetTasks, o.BudgetUntil)
	}
	if d := by(a, deepseek); d == nil || d.ReasoningEffort != "high" || d.Basis != KindUnmetered {
		t.Fatalf("deepseek = %+v", d)
	}
}

func TestCodexSurplusUsesXHighAndPriority(t *testing.T) {
	inv := normalFleet()
	inv.Accounts[3] = codexAcct("codex-a", 20, 8*time.Hour, 0)
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if s := by(a, sol); s == nil || s.Basis != KindSurplus || s.ReasoningEffort != "xhigh" || s.Mode != ModePriority {
		t.Fatalf("codex surplus = %+v", s)
	}
	if a.Models[0].Model != sol {
		t.Fatalf("expiring allowance should come first: %+v", a.Models)
	}
}

func TestFastModeOffKeepsStandard(t *testing.T) {
	svc, _, _ := newTestService(t, Config{FastMode: "off"}, surplusFleet())
	for _, m := range mustAdvise(t, svc, Request{}).Models {
		if m.Mode != ModeStandard {
			t.Fatalf("fast mode used while off: %+v", m)
		}
	}
}

func TestAccountRemovalAndAdditionNeedNoRestart(t *testing.T) {
	svc, src, _ := newTestService(t, Config{}, normalFleet())
	src.mu.Lock()
	src.inv.Accounts = []Account{codexAcct("codex-a", 30, 96*time.Hour, 0.5), opencodeAcct("opencode")}
	src.mu.Unlock()
	a := mustAdvise(t, svc, Request{})
	if by(a, opus) != nil || len(a.Models) != 2 || len(a.Warnings) == 0 {
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
	cfg := Config{Models: []ModelCapability{{Name: "gemini-pro", Match: []string{"gemini-4-pro*"}, Provider: "gemini", Efforts: []string{"high", "max"}, Harnesses: map[string]string{"gemini-cli": ""}}}}
	svc, _, _ := newTestService(t, cfg, inv)
	a := mustAdvise(t, svc, Request{})
	if len(a.Models) != 1 {
		t.Fatalf("models = %+v", a.Models)
	}
	// max is configured but the registry does not advertise it: never selected.
	if m := a.Models[0]; m.Model != "gemini-4-pro" || m.ReasoningEffort != "high" || m.CapabilitySource != "registry+configured" || m.Harnesses["gemini-cli"] != "gemini-4-pro" {
		t.Fatalf("gemini = %+v", m)
	}
}

func TestMissingCapabilityIsNeverSelected(t *testing.T) {
	inv := normalFleet()
	inv.Models[2].Levels = []string{"low", "medium"}
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if by(a, sol) != nil || by(a, deepseek) == nil {
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

func TestExhaustedAccountsAndRetryAfter(t *testing.T) {
	inv := normalFleet()
	for i := 0; i < 3; i++ {
		inv.Accounts[i].Windows[0].UsedPercent = 100
	}
	svc, src, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if by(a, opus) != nil || len(a.Models) != 2 {
		t.Fatalf("exhausted claude advised: %+v", a.Models)
	}
	// Everything unavailable: no invented capacity, a retry hint instead.
	src.mu.Lock()
	src.inv.Accounts = src.inv.Accounts[:3]
	src.mu.Unlock()
	a = mustAdvise(t, svc, Request{})
	if a.Status != StatusUnavailable || a.Posture != PostureUnavailable || a.RetryAfterSeconds == nil || len(a.Models) != 0 {
		t.Fatalf("advice = %s %s retry %v", a.Status, a.Posture, a.RetryAfterSeconds)
	}
	if want := int((72 * time.Hour).Seconds()); *a.RetryAfterSeconds != want || a.RefreshAfterSeconds != 3600 || !strings.Contains(a.RefreshReason, "further out") {
		t.Fatalf("retry after %d (want %d), refresh %d %q", *a.RetryAfterSeconds, want, a.RefreshAfterSeconds, a.RefreshReason)
	}
}

func TestRefreshFollowsTheSituation(t *testing.T) {
	refresh := func(inv Inventory) Advice {
		t.Helper()
		svc, _, _ := newTestService(t, Config{}, inv)
		return mustAdvise(t, svc, Request{})
	}
	a := refresh(normalFleet())
	if a.RefreshAfterSeconds != 3600 || !a.ValidUntil.Equal(t0.Add(time.Hour)) || !a.GeneratedAt.Equal(t0) || !strings.Contains(a.RefreshReason, "comfortable") {
		t.Fatalf("comfortable: %d %q valid until %s", a.RefreshAfterSeconds, a.RefreshReason, a.ValidUntil)
	}
	if a = refresh(surplusFleet()); a.RefreshAfterSeconds != 900 || !strings.Contains(a.RefreshReason, "expire") {
		t.Fatalf("surplus: %d %q", a.RefreshAfterSeconds, a.RefreshReason)
	}
	if a = refresh(Inventory{Accounts: []Account{claudeAcct("claude-a", 95, 72*time.Hour, 2)}, Models: discovered()}); a.RefreshAfterSeconds != 600 || !strings.Contains(a.RefreshReason, "tight") {
		t.Fatalf("tight: %d %q", a.RefreshAfterSeconds, a.RefreshReason)
	}
	inv := normalFleet()
	inv.Accounts[4].Blocked, inv.Accounts[4].BlockedUntil = "rate_limited", t0.Add(25*time.Minute)
	if a = refresh(inv); a.RefreshAfterSeconds != 1500 || a.RefreshReason != "an account recovers from rate_limited" {
		t.Fatalf("recovery: %d %q", a.RefreshAfterSeconds, a.RefreshReason)
	}
	inv = normalFleet()
	inv.Accounts[1].Windows[1].UsedPercent = 90
	inv.Accounts[1].Windows[1].ResetAt = t0.Add(40 * time.Minute)
	if a = refresh(inv); a.RefreshAfterSeconds != 2400 || a.RefreshReason != "a 5h window reopens" {
		t.Fatalf("5h window: %d %q", a.RefreshAfterSeconds, a.RefreshReason)
	}
	inv = normalFleet()
	for i := range inv.Accounts {
		inv.Accounts[i].Blocked, inv.Accounts[i].BlockedUntil = "rate_limited", t0.Add(7*time.Minute)
	}
	if a = refresh(inv); a.Status != StatusUnavailable || a.RefreshAfterSeconds != 420 {
		t.Fatalf("unavailable: %s %d %q", a.Status, a.RefreshAfterSeconds, a.RefreshReason)
	}
	for i := range inv.Accounts {
		inv.Accounts[i].BlockedUntil = t0.Add(5 * time.Second)
	}
	if a = refresh(inv); a.RefreshAfterSeconds != 120 || *a.RetryAfterSeconds != 5 {
		t.Fatalf("minimum: %d retry %v", a.RefreshAfterSeconds, *a.RetryAfterSeconds)
	}
	svc, _, _ := newTestService(t, Config{MaxRefreshSeconds: 1800}, normalFleet())
	if a = mustAdvise(t, svc, Request{}); a.RefreshAfterSeconds != 1800 {
		t.Fatalf("configured maximum: %d", a.RefreshAfterSeconds)
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
	if by(a, sol) != nil || by(a, opus) == nil || by(a, deepseek) == nil {
		t.Fatalf("fallback advice = %+v", a.Models)
	}
}

func TestStaleAndExpiredObservations(t *testing.T) {
	inv := surplusFleet()
	inv.Accounts[0].Windows[0].ObservedAt = t0.Add(-30 * time.Minute)
	svc, src, _ := newTestService(t, Config{}, inv)
	f, err := svc.Forecast(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var stale AccountForecast
	for _, acct := range f.Accounts {
		if acct.AccountRef == accountRef("claude", "claude-a") {
			stale = acct
		}
	}
	if !stale.Stale || stale.PaceConfidence >= 0.85 {
		t.Fatalf("stale account forecast = %+v", stale)
	}
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
	if o := by(a, opus); o == nil || o.Basis != KindUnmetered || o.Mode != ModeStandard || o.BudgetTasks != nil {
		t.Fatalf("expired evidence produced %+v", o)
	}
	if a.Freshness.UnmeteredAccounts != 4 {
		t.Fatalf("unmetered = %d", a.Freshness.UnmeteredAccounts)
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

func TestZeroHistoryNeverClaimsSurplus(t *testing.T) {
	inv := surplusFleet()
	for i := range inv.Accounts {
		inv.Accounts[i].History = nil
	}
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	noSurplus(t, a, "surplus claimed without pace evidence")
	if len(a.Models) != 3 {
		t.Fatalf("every healthy model should be listed, got %d", len(a.Models))
	}
}

func TestLedgerPaceAfterRestart(t *testing.T) {
	inv := surplusFleet()
	for i := range inv.Accounts {
		inv.Accounts[i].History = nil
	}
	// claude-a was idle in the ledger; b and c used a little.
	inv.Accounts[0].Usage = &UsageHistory{}
	inv.Accounts[1].Usage = &UsageHistory{Last24h: []ModelUsage{{Model: opus, InputTokens: 1e6, OutputTokens: 1e5}}}
	inv.Accounts[2].Usage = &UsageHistory{}
	svc, _, _ := newTestService(t, Config{}, inv)
	if o := by(mustAdvise(t, svc, Request{}), opus); o == nil || o.Basis != KindSurplus {
		t.Fatalf("ledger pace should reveal claude-a surplus: %+v", o)
	}
}

func TestSpikeReducesSurplus(t *testing.T) {
	inv := surplusFleet()
	acct := &inv.Accounts[0]
	// Idle for 5h, then 30 points in the last 1.5h: pace now 20pp/h.
	acct.Windows[0].UsedPercent = 70
	acct.History = []Sample{
		{At: t0.Add(-6 * time.Hour), UsedPercent: 40, ResetAt: acct.Windows[0].ResetAt},
		{At: t0.Add(-90 * time.Minute), UsedPercent: 40, ResetAt: acct.Windows[0].ResetAt},
	}
	svc, _, _ := newTestService(t, Config{}, inv)
	noSurplus(t, mustAdvise(t, svc, Request{}), "spike should remove projected surplus")
}

func TestShortWindowCautionAndClosure(t *testing.T) {
	inv := surplusFleet()
	inv.Accounts[0].Windows[1].UsedPercent = 90
	svc, src, _ := newTestService(t, Config{}, inv)
	noSurplus(t, mustAdvise(t, svc, Request{}), "surplus or fast advice despite 5h caution")
	src.mu.Lock()
	src.inv.Accounts[0].Windows[1].Full = true
	src.mu.Unlock()
	a := mustAdvise(t, svc, Request{})
	noSurplus(t, a, "closed 5h window account used for surplus")
	if by(a, opus) == nil {
		t.Fatalf("other claude accounts should still serve opus: %+v", a.Models)
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

func TestRoutingOverrideDisablesSurplus(t *testing.T) {
	inv := surplusFleet()
	inv.RoutingOverrides = map[string]string{"claude": "single"}
	svc, _, _ := newTestService(t, Config{}, inv)
	if o := by(mustAdvise(t, svc, Request{}), opus); o == nil || o.Basis == KindSurplus {
		t.Fatalf("override ignored: %+v", o)
	}
}

func TestTightPoolConserves(t *testing.T) {
	// One Claude account, 5% left, burning 2pp/h, reset in 3 days.
	inv := Inventory{Accounts: []Account{claudeAcct("claude-a", 95, 72*time.Hour, 2)}, Models: discovered()}
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	if a.Status != StatusDegraded || a.Posture != PostureConserve || len(a.Models) != 1 || a.Models[0].Basis != KindConserve {
		t.Fatalf("conserve advice = %s %s %+v", a.Status, a.Posture, a.Models)
	}
	if !strings.HasPrefix(a.Text(), "conserve: ") {
		t.Fatalf("text = %s", a.Text())
	}
}

func TestInFlightRequestsReduceCapacity(t *testing.T) {
	inv := Inventory{Accounts: []Account{claudeAcct("claude-a", 40, 10*time.Hour, 0)}, Models: discovered()}
	inv.Accounts[0].ActiveRequests = 1200 // 0.05pp each: 60pp in flight
	svc, _, _ := newTestService(t, Config{}, inv)
	noSurplus(t, mustAdvise(t, svc, Request{}), "in-flight work should consume the surplus")
}

func TestStaleSingleObservationIsNotZeroPace(t *testing.T) {
	inv := surplusFleet()
	for i := range inv.Accounts {
		inv.Accounts[i].History = nil
		for j := range inv.Accounts[i].Windows {
			inv.Accounts[i].Windows[j].ObservedAt = t0.Add(-2 * time.Hour)
		}
	}
	svc, _, now := newTestService(t, Config{}, inv)
	// The sampler stored the observation long ago; no newer one exists.
	*now = t0.Add(-2 * time.Hour)
	if err := svc.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = t0
	noSurplus(t, mustAdvise(t, svc, Request{}), "surplus invented from a single stale observation")
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

func TestTextIsShortAndComplete(t *testing.T) {
	svc, src, _ := newTestService(t, Config{}, surplusFleet())
	a := mustAdvise(t, svc, Request{})
	text := a.Text()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 4 || len(text) > 700 {
		t.Fatalf("text should be one posture line plus one line per model:\n%s", text)
	}
	if !strings.HasPrefix(lines[0], "spend freely: allowance would expire unused, so many parallel subagents and fast mode are welcome | reuse until 2026-11-12T12:15:00Z (") {
		t.Fatalf("posture line = %q", lines[0])
	}
	budget := roughCount(*by(a, opus).BudgetTasks)
	if lines[1] != "claude-opus-5-5 effort=xhigh mode=fast | ~"+budget+" subagent tasks before reset in 10h | claude-code: opus, opencode: claude-opus-5-5" {
		t.Fatalf("opus line = %q", lines[1])
	}
	if !strings.Contains(lines[3], "budget unknown") {
		t.Fatalf("unmetered line = %q", lines[3])
	}
	src.mu.Lock()
	src.inv.Accounts = nil
	src.mu.Unlock()
	if text = mustAdvise(t, svc, Request{}).Text(); !strings.HasPrefix(text, "unavailable: start no new subagents | reuse until ") {
		t.Fatalf("unavailable text = %q", text)
	}
	for n, want := range map[int]string{7: "7", 99: "99", 123: "120", 2950: "2900", 12345: "12000"} {
		if got := roughCount(n); got != want {
			t.Fatalf("roughCount(%d) = %s, want %s", n, got, want)
		}
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
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Efforts: []string{"ultra"}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Efforts: []string{"high"}, Modes: []string{"turbo"}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"["}, Efforts: []string{"high"}}}},
		{SafetyFactor: math.NaN()},
		{SurplusHorizonHours: math.Inf(1)},
		{Providers: map[string]ProviderConfig{"claude": {CapacityUnits: math.Inf(1)}}},
		{AccountCapacityUnits: map[string]float64{"a": math.NaN()}},
		{TaskInputTokens: -1},
		{TaskOutputTokens: -5},
		{Prices: map[string]Price{"x*": {}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Efforts: []string{"high"}, Price: &Price{Input: -1, Output: 2}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Efforts: []string{"high"}, ModeCostMultiplier: map[string]float64{"fast": 0}}}},
	}
	for i, cfg := range bad {
		if cfg.Validate() == nil {
			t.Fatalf("config %d accepted", i)
		}
	}
	r := Config{Providers: map[string]ProviderConfig{"codex": {PlanCapacityUnits: map[string]float64{"Pro": 500}}}}.Resolve()
	if r.Providers["codex"].PlanCapacityUnits["pro"] != 500 || r.Providers["claude"].CapacityUnits != 13000 {
		t.Fatalf("resolve merge = %+v", r.Providers)
	}
	defaults := DefaultConfig().Resolve()
	if len(defaults.NeverAdvise) != 1 || defaults.NeverAdvise[0] != "claude-fable-*" {
		t.Fatalf("never-advise default = %v", defaults.NeverAdvise)
	}
	for _, m := range defaults.Models {
		if m.Name == "claude-opus-5.5" && (len(m.Efforts) != 1 || m.Efforts[0] != "xhigh") {
			t.Fatalf("opus efforts = %v", m.Efforts)
		}
		if matchModel(defaults.NeverAdvise, m.Match[0]) {
			t.Fatalf("default model %s is on the never-advise list", m.Name)
		}
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
		{Name: "prefixed", Match: []string{"team/claude-fable-*"}, Provider: "claude", Efforts: []string{"high"}},
		{Name: "alias", Match: []string{"best"}, Provider: "claude", Efforts: []string{"high"}},
		{Name: "opus", Match: []string{opus}, Provider: "claude", Efforts: []string{"xhigh"}},
	}}
	svc, _, _ := newTestService(t, cfg, Inventory{Accounts: []Account{acct}, Models: discovered()})
	a := mustAdvise(t, svc, Request{})
	if len(a.Models) != 1 || a.Models[0].Model != opus {
		t.Fatalf("fable advised through a prefix or alias: %+v", a.Models)
	}
}

func TestSurplusBudgetStaysAboveReserves(t *testing.T) {
	inv := surplusFleet()
	// 60% of 13000 left; a 50% reserve leaves 1300 units: 270 fast xhigh tasks.
	inv.Accounts[0].ReservePercent = 50
	svc, _, _ := newTestService(t, Config{}, inv)
	o := by(mustAdvise(t, svc, Request{}), opus)
	if o == nil || o.Basis != KindSurplus || o.BudgetTasks == nil || *o.BudgetTasks != 270 {
		t.Fatalf("surplus budget ignored the reserve: %+v", o)
	}
}

func TestConserveBudgetStaysAboveReserves(t *testing.T) {
	inv := Inventory{Accounts: []Account{claudeAcct("claude-a", 95, 72*time.Hour, 2)}, Models: discovered()}
	// 650 units left, 520 reserved: 130 units at 2.4 per task.
	inv.Accounts[0].ReservePercent = 4
	svc, _, _ := newTestService(t, Config{}, inv)
	o := by(mustAdvise(t, svc, Request{}), opus)
	if o == nil || o.Basis != KindConserve || o.BudgetTasks == nil || *o.BudgetTasks != 54 {
		t.Fatalf("conserve budget ignored the reserve: %+v", o)
	}
}

func TestCreditFundedAccountsStayAdvised(t *testing.T) {
	acct := codexAcct("codex-a", 100, 96*time.Hour, 0)
	acct.ServingOnCredits = true
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	a := mustAdvise(t, svc, Request{})
	s := by(a, sol)
	if a.Posture != PostureConserve || s == nil || s.Basis != KindConserve || s.BudgetTasks != nil {
		t.Fatalf("credit-funded account advice = %s %+v", a.Posture, a.Models)
	}
}

func TestMixedPricingBudgetUsesEachAccountsCost(t *testing.T) {
	a := codexAcct("codex-a", 99, 150*time.Hour, 0)
	b := codexAcct("codex-b", 0, 160*time.Hour, 0)
	b.Plan = "plus"
	cfg := Config{Providers: map[string]ProviderConfig{"codex": {PlanCapacityUnits: map[string]float64{"pro": 5000}}}}
	svc, _, _ := newTestService(t, cfg, Inventory{Accounts: []Account{a, b}, Models: discovered()})
	// codex-a: 50 units at 1.35 per task = 37; codex-b (pool minimum, 2% per task) = 50.
	s := by(mustAdvise(t, svc, Request{}), sol)
	if s == nil || s.BudgetTasks == nil || *s.BudgetTasks != 87 {
		t.Fatalf("mixed pricing budget = %+v", s)
	}
}

func TestUnavailableFirstMatchFallsThrough(t *testing.T) {
	acct := Account{ID: "opencode", Provider: "opencode-go", Models: []string{"opencode-go/deepseek-v4.1", deepseek},
		ModelBlocks: map[string]Block{"opencode-go/deepseek-v4.1": {Reason: "model_cooldown", Until: t0.Add(time.Hour)}}}
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
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
	cfg := Config{Models: []ModelCapability{{Name: "best", Match: []string{"best"}, Provider: "claude", Efforts: []string{"high"}}}}
	svc, _, _ := newTestService(t, cfg, Inventory{Accounts: []Account{a, b}, Models: discovered()})
	if adv := mustAdvise(t, svc, Request{}); len(adv.Models) != 0 {
		t.Fatalf("a shared alias that routes to Fable was advised: %+v", adv.Models)
	}
}

func TestCreditsCoverFullOrdinaryWindows(t *testing.T) {
	acct := codexAcct("codex-a", 100, 96*time.Hour, 0)
	acct.ServingOnCredits = true
	acct.Windows = append(acct.Windows, Window{Name: "5h", UsedPercent: 100, Full: true, ResetAt: t0.Add(2 * time.Hour), DurationSeconds: 18000, ObservedAt: t0.Add(-time.Minute)})
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	if s := by(mustAdvise(t, svc, Request{}), sol); s == nil || s.Basis != KindConserve || s.BudgetTasks != nil {
		t.Fatalf("credit-funded account with a full 5h window = %+v", s)
	}
	// Without credits the same full window blocks.
	acct.ServingOnCredits = false
	svc, _, _ = newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	if s := by(mustAdvise(t, svc, Request{}), sol); s != nil {
		t.Fatalf("full window without credits was advised: %+v", s)
	}
}
