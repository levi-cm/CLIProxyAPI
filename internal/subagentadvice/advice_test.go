package subagentadvice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/fleetactivity"
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

// normalFleet's demand uses every weekly allowance before it resets.
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
	tracker := fleetactivity.New()
	tracker.SetClock(func() time.Time { return now })
	svc.SetActivity(tracker)
	return svc, src, &now
}

// working records n agents (distinct sessions) on provider in the last minutes.
func working(svc *Service, provider string, n int) {
	for i := 0; i < n; i++ {
		r := svc.activity.Begin("scope-secret", fmt.Sprintf("session-%s-%d", provider, i))
		r.Provider(provider)
		r.End()
	}
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
	inv.Models[2].Levels = []string{"low"}
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

func TestLiveClaudeAdvisesHighWithRoomForAboutOneMore(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, liveClaude())
	working(svc, "claude", 4)
	a := mustAdvise(t, svc, Request{})
	o := by(a, opus)
	if a.Posture != PostureRoom || o == nil || o.ReasoningEffort != "high" || o.Mode != ModeStandard || o.Basis != KindRoom || o.Step != 1 || o.Steps != 3 {
		t.Fatalf("advice = %s %+v", a.Posture, o)
	}
	// need = 1/91 + 92/128 + 100/140 = 1.44%/h; target 0.8x = 1.16%/h; recent
	// 0.9%/h over 4 agents = 0.225%/h each: floor(0.26/0.225) = 1 more.
	fr := a.Providers[0].Fleet
	if fr == nil || fr.RoomAgents != 1 || o.RoomAgents == nil || *o.RoomAgents != 1 || a.RoomAgents != 1 || fr.PerAgentBasis != "measured" || fr.RecentBasis != PaceObserved {
		t.Fatalf("fleet = %+v", fr)
	}
	if math.Abs(fr.NeedPercentPerHour-1.444) > 0.01 || math.Abs(fr.TargetPercentPerHour-1.155) > 0.01 || math.Abs(fr.RecentPercentPerHour-0.9) > 0.01 || fr.TimeSensitive {
		t.Fatalf("rates = %+v", fr)
	}
	if a.Fleet.Agents != 4 || a.Fleet.Providers["claude"].Agents != 4 {
		t.Fatalf("activity = %+v", a.Fleet)
	}
	if !strings.HasPrefix(a.Text(), "room for ~1 more agent across all orchestrators (4 working now); start at most that many | reuse until 2026-11-12T12:05:00Z") {
		t.Fatalf("text = %s", a.Text())
	}
}

func TestFleetAtTargetHolds(t *testing.T) {
	inv := liveClaude()
	inv.Accounts[1] = claudeAcct("claude-b", 8, 128*time.Hour, 1.3)
	svc, _, _ := newTestService(t, Config{}, inv)
	working(svc, "claude", 2)
	a := mustAdvise(t, svc, Request{})
	o := by(a, opus)
	if a.Posture != PostureHold || o.Basis != KindHold || o.ReasoningEffort != "high" || o.RoomAgents == nil || *o.RoomAgents != 0 || a.RoomAgents != 0 {
		t.Fatalf("advice = %s %+v", a.Posture, o)
	}
	if !strings.HasPrefix(a.Text(), "hold: 2 agents working already use the target rate; start no new agents | ") {
		t.Fatalf("text = %s", a.Text())
	}
	// A lower target fraction holds sooner; a higher one makes room.
	svc, _, _ = newTestService(t, Config{TargetFraction: 1.5}, inv)
	working(svc, "claude", 2)
	if a = mustAdvise(t, svc, Request{}); a.Posture != PostureRoom {
		t.Fatalf("target-fraction 1.5 = %s %+v", a.Posture, a.Providers[0].Fleet)
	}
}

func TestFleetAboveNeedConserves(t *testing.T) {
	// One Claude account, 5% left, burning 2pp/h, reset in 3 days.
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{claudeAcct("claude-a", 95, 72*time.Hour, 2)}, Models: discovered()})
	a := mustAdvise(t, svc, Request{})
	if a.Status != StatusDegraded || a.Posture != PostureConserve || len(a.Models) != 1 || a.Models[0].Basis != KindConserve || a.Models[0].Step != 1 {
		t.Fatalf("conserve advice = %s %s %+v", a.Status, a.Posture, a.Models)
	}
	if !strings.HasPrefix(a.Text(), "conserve: ") {
		t.Fatalf("text = %s", a.Text())
	}
}

func TestLadderClimbsOnlyWellUnderTarget(t *testing.T) {
	step := func(pace float64) ModelAdvice {
		t.Helper()
		inv := liveClaude()
		inv.Accounts[1] = claudeAcct("claude-b", 8, 128*time.Hour, pace)
		svc, _, _ := newTestService(t, Config{}, inv)
		working(svc, "claude", 1)
		return *by(mustAdvise(t, svc, Request{}), opus)
	}
	// Target ~1.16%/h: 0.8 is 69% of it, 0.5 is 43%, 0.2 is 17%.
	if o := step(0.8); o.ReasoningEffort != "high" || o.Step != 1 {
		t.Fatalf("near target = %+v", o)
	}
	if o := step(0.5); o.ReasoningEffort != "xhigh" || o.Mode != ModeStandard || o.Step != 2 {
		t.Fatalf("under 0.6 of target = %+v", o)
	}
	// Two steps up would be fast mode, which only time-sensitive surplus unlocks.
	if o := step(0.2); o.ReasoningEffort != "xhigh" || o.Mode != ModeStandard || o.Step != 2 || !strings.Contains(o.Reason, "17% of its target") {
		t.Fatalf("under 0.3 of target = %+v", o)
	}
	// Codex climbs medium -> high -> xhigh, never to priority by itself.
	inv := Inventory{Accounts: []Account{codexAcct("codex-a", 20, 96*time.Hour, 0.1), codexAcct("codex-b", 0, 150*time.Hour, 0)}, Models: discovered()}
	svc, _, _ := newTestService(t, Config{}, inv)
	working(svc, "codex", 1)
	if s := by(mustAdvise(t, svc, Request{}), sol); s.ReasoningEffort != "xhigh" || s.Mode != ModeStandard || s.Step != 3 || s.Steps != 4 {
		t.Fatalf("codex = %+v", s)
	}
	inv.Accounts[0] = codexAcct("codex-a", 20, 96*time.Hour, 1.2)
	svc, _, _ = newTestService(t, Config{}, inv)
	working(svc, "codex", 1)
	if s := by(mustAdvise(t, svc, Request{}), sol); s.ReasoningEffort != "medium" || s.Step != 1 {
		t.Fatalf("codex near target = %+v", s)
	}
}

// expiringFleet has two idle Claude accounts whose weeks end within a day.
func expiringFleet() Inventory {
	return Inventory{Accounts: []Account{
		claudeAcct("claude-a", 0, 10*time.Hour, 0),
		claudeAcct("claude-b", 0, 12*time.Hour, 0),
		claudeAcct("claude-c", 50, 140*time.Hour, 0),
	}, Models: discovered()}
}

func TestFastStepOnlyForTimeSensitiveSurplus(t *testing.T) {
	// At a 0.5 target the fleet would still leave ~90% of one account
	// unused at the resets within 24h: the top (fast) step applies.
	svc, _, _ := newTestService(t, Config{TargetFraction: 0.5}, expiringFleet())
	a := mustAdvise(t, svc, Request{})
	o := by(a, opus)
	if o.ReasoningEffort != "xhigh" || o.Mode != ModeFast || o.Step != 3 || !a.Providers[0].Fleet.TimeSensitive || !strings.Contains(o.Reason, "expire unused within 24h") {
		t.Fatalf("fast = %+v fleet %+v", o, a.Providers[0].Fleet)
	}
	if !strings.Contains(a.Text(), "claude-opus-5-5 effort=xhigh mode=fast | extra high + fast, step 3 of 3 | room for ~") {
		t.Fatalf("text = %s", a.Text())
	}
	// At the default 0.8 target the fleet would use nearly all of it.
	svc, _, _ = newTestService(t, Config{}, expiringFleet())
	if o = by(mustAdvise(t, svc, Request{}), opus); o.Mode != ModeStandard {
		t.Fatalf("default target = %+v", o)
	}
	// fast-mode off, a nearly full 5h window or a routing override rule it out.
	for name, mutate := range map[string]func(*Config, *Inventory){
		"off":      func(c *Config, _ *Inventory) { c.FastMode = "off" },
		"caution":  func(_ *Config, inv *Inventory) { inv.Accounts[0].Windows[1].UsedPercent = 90 },
		"override": func(_ *Config, inv *Inventory) { inv.RoutingOverrides = map[string]string{"claude": "single"} },
	} {
		cfg, inv := Config{TargetFraction: 0.5}, expiringFleet()
		mutate(&cfg, &inv)
		svc, _, _ = newTestService(t, cfg, inv)
		if o = by(mustAdvise(t, svc, Request{}), opus); o.Mode != ModeStandard {
			t.Fatalf("%s: %+v", name, o)
		}
	}
}

func TestCreditsCountAsRoomButNeverTriggerPriority(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, liveCodex(true))
	working(svc, "codex", 1)
	adv := mustAdvise(t, svc, Request{})
	s := by(adv, sol)
	fr := adv.Providers[0].Fleet
	// ~832% of one account in credits over the 192h horizon: ~4.3%/h.
	if s == nil || s.Mode != ModeStandard || fr.TimeSensitive || fr.CreditsPercentPerHour < 4 || fr.State != RateRoom || fr.RoomAgents < 3 {
		t.Fatalf("credit advice = %+v fleet %+v", s, fr)
	}
	without, _, _ := newTestService(t, Config{}, liveCodex(false))
	working(without, "codex", 1)
	if fr2 := mustAdvise(t, without, Request{}).Providers[0].Fleet; fr2.CreditsPercentPerHour != 0 || fr2.RoomAgents >= fr.RoomAgents {
		t.Fatalf("credits counted while routing ignores them: %+v", fr2)
	}
	text := adv.Text()
	if !strings.Contains(text, "; credits above the 7.5k floors: 107k (+416%), spend them; 6 full-reset grants (earliest expires in 18d), redeemed automatically\n") ||
		!strings.Contains(text, "incl. 4.3%/h of credits") {
		t.Fatalf("text = %s", text)
	}
}

func TestPerAgentFallsBackToTaskEstimate(t *testing.T) {
	inv := liveClaude()
	for i := range inv.Accounts {
		inv.Accounts[i].History = nil
	}
	svc, _, _ := newTestService(t, Config{}, inv)
	working(svc, "claude", 3)
	a := mustAdvise(t, svc, Request{})
	fr := a.Providers[0].Fleet
	// A high task: (300k*$4 + 40k*$20)/1e6 = 2 units of 13000 per 30 minutes.
	if fr.PerAgentBasis != "task_estimate" || fr.RecentBasis != "estimated" || math.Abs(fr.PerAgentPercentPerHour-0.0308) > 0.001 || math.Abs(fr.RecentPercentPerHour-3*0.0308) > 0.003 {
		t.Fatalf("fallback = %+v", fr)
	}
	if o := by(a, opus); !strings.Contains(o.Reason, "estimated from task size") || !strings.Contains(a.Text(), "(estimated from task size)") {
		t.Fatalf("fallback not explained: %+v\n%s", o, a.Text())
	}
	// A ledger rate after a restart counts as measured use.
	for i := range inv.Accounts {
		inv.Accounts[i].Usage = &UsageHistory{}
	}
	inv.Accounts[1].Usage = &UsageHistory{Last3h: []ModelUsage{{Model: opus, InputTokens: 3e6, OutputTokens: 3e5}}}
	svc, _, _ = newTestService(t, Config{}, inv)
	working(svc, "claude", 2)
	if fr = mustAdvise(t, svc, Request{}).Providers[0].Fleet; fr.RecentBasis != PaceLedger || fr.PerAgentBasis != "measured" || fr.RecentPercentPerHour <= 0 {
		t.Fatalf("ledger = %+v", fr)
	}
}

func TestReserveLowersNeed(t *testing.T) {
	inv := Inventory{Accounts: []Account{claudeAcct("claude-a", 40, 60*time.Hour, 0.2)}, Models: discovered()}
	svc, _, _ := newTestService(t, Config{}, inv)
	free := mustAdvise(t, svc, Request{}).Providers[0].Fleet
	inv.Accounts[0].ReservePercent = 30
	svc, _, _ = newTestService(t, Config{}, inv)
	reserved := mustAdvise(t, svc, Request{}).Providers[0].Fleet
	if math.Abs(free.NeedPercentPerHour-1) > 0.01 || math.Abs(reserved.NeedPercentPerHour-0.5) > 0.01 {
		t.Fatalf("need %v / %v", free.NeedPercentPerHour, reserved.NeedPercentPerHour)
	}
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
	if o := by(a, opus); o == nil || o.Basis != KindUnmetered || o.Mode != ModeStandard || o.RoomAgents != nil || a.Posture != PostureUnknown {
		t.Fatalf("expired evidence produced %s %+v", a.Posture, o)
	}
	if !strings.HasPrefix(a.Text(), "budget unknown: ") || !strings.Contains(a.Text(), "| budget unknown |") {
		t.Fatalf("text = %s", a.Text())
	}
}

func TestCreditFundedAccountsStayAdvised(t *testing.T) {
	acct := codexAcct("codex-a", 100, 96*time.Hour, 0)
	acct.ServingOnCredits = true
	acct.Windows = append(acct.Windows, Window{Name: "5h", UsedPercent: 100, Full: true, ResetAt: t0.Add(2 * time.Hour), DurationSeconds: 18000, ObservedAt: t0.Add(-time.Minute)})
	acct.Credits = &Credits{Balance: 20000, Spendable: 12500, Floor: 7500, Enabled: true}
	svc, _, _ := newTestService(t, Config{}, Inventory{Accounts: []Account{acct}, Models: discovered()})
	a := mustAdvise(t, svc, Request{})
	if s := by(a, sol); s == nil || s.Basis == KindConserve || a.Providers[0].Fleet.CreditsPercentPerHour <= 0 {
		t.Fatalf("credit-funded account = %s %+v", a.Posture, s)
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
	if by(a, opus) != nil || len(a.Models) != 2 {
		t.Fatalf("exhausted claude advised: %+v", a.Models)
	}
	src.mu.Lock()
	src.inv.Accounts = src.inv.Accounts[:3]
	src.mu.Unlock()
	a = mustAdvise(t, svc, Request{})
	if a.Status != StatusUnavailable || a.Posture != PostureUnavailable || a.RetryAfterSeconds == nil || len(a.Models) != 0 {
		t.Fatalf("advice = %s %s retry %v", a.Status, a.Posture, a.RetryAfterSeconds)
	}
	if want := int((72 * time.Hour).Seconds()); *a.RetryAfterSeconds != want || a.RefreshAfterSeconds != 300 {
		t.Fatalf("retry after %d (want %d), refresh %d %q", *a.RetryAfterSeconds, want, a.RefreshAfterSeconds, a.RefreshReason)
	}
	if !strings.HasPrefix(a.Text(), "unavailable: start no new subagents | reuse until ") {
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

func TestAdviceReservesAndCapsNothing(t *testing.T) {
	svc, _, _ := newTestService(t, Config{}, liveClaude())
	working(svc, "claude", 4)
	raw, err := json.Marshal(mustAdvise(t, svc, Request{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_parallel", "max_subagents", "lease", "reservation", "budget_tasks"} {
		if strings.Contains(string(raw), "\""+field+"\"") {
			t.Fatalf("advice carries %q: %s", field, raw)
		}
	}
	for _, leaked := range []string{"claude-a", "scope-secret", "session-claude"} {
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

func TestOpenCodeGoAvoidedWhenTrackerSaysSo(t *testing.T) {
	srv := trackerServer(t, trackerLow)
	svc, _, _ := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := mustAdvise(t, svc, Request{})
	if by(a, deepseek) != nil {
		t.Fatalf("opencode advised while nearly empty: %+v", a.Models)
	}
	want := "OpenCode Go: avoid until Nov 21 12:00 UTC (monthly 2% left)"
	if !strings.Contains(a.Text(), want+"\n") || !strings.Contains(a.Text(), "warning: "+want+"\n") {
		t.Fatalf("text = %s", a.Text())
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
}

func TestOpenCodeGoUsableShowsWindows(t *testing.T) {
	srv := trackerServer(t, trackerOK)
	svc, _, now := newTestService(t, Config{OpenCodeUsageURL: srv.URL}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := mustAdvise(t, svc, Request{})
	d := by(a, deepseek)
	if d == nil || d.Basis != KindUnmetered || d.ReasoningEffort != "high" {
		t.Fatalf("deepseek = %+v", d)
	}
	text := a.Text()
	if !strings.Contains(text, "OpenCode Go: 5h 90% left, resets in 1h (stale); monthly 40% left, resets in 9d\n") || !strings.Contains(text, "room unknown (quota per tracker above)") || strings.Contains(text, "warning: OpenCode") {
		t.Fatalf("text = %s", text)
	}
	// A reading older than 15 minutes counts as unreachable.
	*now = t0.Add(20 * time.Minute)
	a = mustAdvise(t, svc, Request{})
	if by(a, deepseek) == nil || !strings.Contains(a.Text(), "warning: OpenCode Go usage tracker unreachable; its budget is unknown") || !strings.Contains(a.Text(), "| budget unknown |") {
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
		if a := mustAdvise(t, svc, Request{}); by(a, deepseek) == nil || !strings.Contains(a.Text(), "tracker unreachable") {
			t.Fatalf("%s: %s", name, a.Text())
		}
	}
	// Without a URL nothing is fetched and nothing warns.
	svc, _, _ := newTestService(t, Config{}, normalFleet())
	if err := svc.SampleOpenCode(context.Background()); err != nil || strings.Contains(mustAdvise(t, svc, Request{}).Text(), "OpenCode") {
		t.Fatalf("unconfigured tracker: %v", err)
	}
}

func TestTextIsShortAndComplete(t *testing.T) {
	inv := liveClaude()
	inv.Accounts = append(inv.Accounts, liveCodex(true).Accounts...)
	inv.Accounts = append(inv.Accounts, opencodeAcct("opencode"))
	svc, _, _ := newTestService(t, Config{}, inv)
	working(svc, "claude", 4)
	working(svc, "codex", 1)
	a := mustAdvise(t, svc, Request{})
	text := a.Text()
	t.Log("\n" + text)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 11 || len(text) > 2600 {
		t.Fatalf("lines = %d, bytes = %d:\n%s", len(lines), len(text), text)
	}
	want := map[int]string{
		1:  "claude weekly, all 3 accounts as one: 64% left now; refills +33% in 3d19h, +3% in 5d8h; projected to expire unused: ",
		2:  "claude accounts: 1% left, resets in 3d19h; 92% left, resets in 5d8h; 100% left, resets in 5d20h",
		3:  "claude fleet: 4 agents working, recent 0.9%/h of one account; target 1.2%/h (80% of the 1.4%/h that uses every account by its reset); ~0.2%/h per agent -> room for ~1 more",
		6:  "codex fleet: 1 agent working, recent ",
		7:  "claude-opus-5-5 effort=high mode=standard | high, step 1 of 3 | room for ~1 more | claude-code: opus, opencode: claude-opus-5-5",
		8:  "gpt-6.1-sol effort=",
		9:  "opencode-go/deepseek-v4.1-flash effort=high mode=standard | high | budget unknown | opencode: opencode-go/deepseek-v4.1-flash",
		10: "agents working (last 10m): 5 (0 requests in flight); claude 4, codex 1; devices key-",
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d = %q, want prefix %q\n%s", i, lines[i], prefix, text)
		}
	}
	if !strings.Contains(lines[8], "extra high") || !strings.Contains(lines[8], "effort=xhigh") {
		t.Fatalf("xhigh must read as extra high but stay xhigh in the token: %q", lines[8])
	}
	if !strings.HasPrefix(lines[0], fmt.Sprintf("room for ~%d more agents across all orchestrators (5 working now); start at most that many | reuse until ", a.RoomAgents)) {
		t.Fatalf("posture line = %q", lines[0])
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
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}, Ladder: []Step{{Effort: "high", Mode: "turbo"}}}}},
		{Models: []ModelCapability{{Name: "x", Match: []string{"x"}}}},
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
		{TargetFraction: -0.1},
		{TargetFraction: math.NaN()},
		{StepUpOneBelow: 0.2, StepUpTwoBelow: 0.5},
		{AgentWindowSeconds: 5},
		{ReuseSeconds: 10},
		{TaskMinutes: -1},
		{FastMinPercent: math.Inf(1)},
		{OpenCodeUsageURL: "ftp://x"},
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
	if len(defaults.NeverAdvise) != 1 || defaults.NeverAdvise[0] != "claude-fable-*" || defaults.TargetFraction != 0.8 || defaults.ReuseSeconds != 300 || defaults.AgentWindowSeconds != 600 || defaults.OpenCodeUsageURL != "" {
		t.Fatalf("defaults = %+v", defaults)
	}
	ladders := map[string]string{}
	for _, m := range defaults.Models {
		var steps []string
		for _, s := range m.Ladder {
			steps = append(steps, s.Effort+"/"+s.Mode)
		}
		ladders[m.Name] = strings.Join(steps, " ")
		if matchModel(defaults.NeverAdvise, m.Match[0]) {
			t.Fatalf("default model %s is on the never-advise list", m.Name)
		}
	}
	if ladders["claude-opus-5.5"] != "high/standard xhigh/standard xhigh/fast" || ladders["gpt-6.1-sol"] != "medium/standard high/standard xhigh/standard xhigh/priority" {
		t.Fatalf("ladders = %v", ladders)
	}
	// Older configs with efforts and default-effort still load as a ladder.
	old := Config{Models: []ModelCapability{{Name: "o", Match: []string{opus}, Provider: "claude", Efforts: []string{"xhigh", "high"}, DefaultEffort: "xhigh", Modes: []string{"fast"}}}}
	if err := old.Validate(); err != nil {
		t.Fatal(err)
	}
	if l := old.Resolve().Models[0].Ladder; len(l) != 2 || l[0].Effort != "high" || l[1].Effort != "xhigh" || l[1].Mode != ModeStandard {
		t.Fatalf("legacy ladder = %+v", l)
	}
}

func TestIdleFleetKeepsLastMeasuredPerAgentRate(t *testing.T) {
	svc, src, now := newTestService(t, Config{}, liveClaude())
	working(svc, "claude", 4)
	if fr := mustAdvise(t, svc, Request{}).Providers[0].Fleet; fr.PerAgentBasis != PerAgentMeasured {
		t.Fatalf("first = %+v", fr)
	}
	// Three idle hours later the recent rate is zero; the last measured
	// per-agent rate (0.225%/h) still sizes the room, not the task estimate.
	*now = t0.Add(3 * time.Hour)
	src.mu.Lock()
	for i := range src.inv.Accounts {
		w := &src.inv.Accounts[i].Windows[0]
		w.ObservedAt = *now
		src.inv.Accounts[i].History = []Sample{{At: t0.Add(30 * time.Minute), UsedPercent: w.UsedPercent, ResetAt: w.ResetAt}}
	}
	src.mu.Unlock()
	a := mustAdvise(t, svc, Request{})
	fr := a.Providers[0].Fleet
	if fr.PerAgentBasis != PerAgentLastMeasured || math.Abs(fr.PerAgentPercentPerHour-0.225) > 0.005 || fr.RecentPercentPerHour != 0 || fr.RoomAgents != 5 {
		t.Fatalf("idle = %+v", fr)
	}
	if !strings.Contains(a.Text(), "~0.2%/h per agent (last measured) -> room for ~5 more") {
		t.Fatalf("text = %s", a.Text())
	}
	// After a day it is forgotten.
	*now = t0.Add(25 * time.Hour)
	src.mu.Lock()
	for i := range src.inv.Accounts {
		src.inv.Accounts[i].Windows[0].ObservedAt = *now
		src.inv.Accounts[i].History = []Sample{{At: t0.Add(22 * time.Hour), UsedPercent: src.inv.Accounts[i].Windows[0].UsedPercent, ResetAt: src.inv.Accounts[i].Windows[0].ResetAt}}
	}
	src.mu.Unlock()
	if fr = mustAdvise(t, svc, Request{}).Providers[0].Fleet; fr.PerAgentBasis != PerAgentTaskEstimate {
		t.Fatalf("after a day = %+v", fr)
	}
}
