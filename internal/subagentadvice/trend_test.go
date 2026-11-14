package subagentadvice

import (
	"math"
	"strings"
	"testing"
	"time"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// The ratio of the trend to the even budget picks the posture: high by
// default, extra high only well under budget, conserve above it, and high
// while the trend is unknown.
func TestTrendRatioPicksPosture(t *testing.T) {
	cases := []struct {
		name                      string
		ratio                     float64
		hours                     int
		posture                   string
		claudeEffort, codexEffort string
	}{
		{"ratio 0.3", 0.3, 72, PostureUseMore, "xhigh", "xhigh"},
		{"ratio 0.8", 0.8, 72, PostureNormal, "high", "high"},
		{"ratio 1.2", 1.2, 72, PostureConserve, "high", "medium"},
		{"unknown", 0.3, 12, PostureNormal, "high", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newTestService(t, Config{}, trendFleet(eachFor(tc.ratio), eachFor(tc.ratio), tc.hours))
			a := mustAdvise(t, svc, Request{})
			o, s := by(a, opus), by(a, sol)
			if o == nil || s == nil {
				t.Fatalf("models = %+v", a.Models)
			}
			if o.Posture != tc.posture || s.Posture != tc.posture || o.ReasoningEffort != tc.claudeEffort || s.ReasoningEffort != tc.codexEffort || o.Mode != ModeStandard || s.Mode != ModeStandard {
				t.Fatalf("opus %+v\nsol %+v", o, s)
			}
			for provider, budget := range map[string]float64{"claude": 300.0 / 7, "codex": 200.0 / 7} {
				p := posture(a, provider)
				if p == nil || p.Posture != tc.posture || p.Trend == nil || !near(p.Trend.BudgetPercentPerDay, budget, 0.01) || p.Trend.PeriodDays != 7 {
					t.Fatalf("%s posture = %+v", provider, p)
				}
				if tc.name == "unknown" {
					if p.Trend.PercentPerDay != nil || p.Trend.Ratio != nil || p.Trend.Unknown != "history covers 12h; needs 24h" {
						t.Fatalf("%s unknown trend = %+v", provider, p.Trend)
					}
					continue
				}
				if p.Trend.Ratio == nil || !near(*p.Trend.Ratio, tc.ratio, 0.01) || p.Trend.Basis != TrendAllowance || !near(p.Trend.CoveredDays, 3, 0.01) {
					t.Fatalf("%s trend = %+v", provider, p.Trend)
				}
			}
		})
	}
}

// Fast (Claude) and priority (Codex) are added only when, at the trend,
// at least half an account of allowance expires unused within a day.
func TestFastOnlyWhenAllowanceExpiresSoon(t *testing.T) {
	fleet := func(ratio float64) Inventory {
		inv := trendFleet(eachFor(ratio), eachFor(ratio), 72)
		// Each pool's first account resets in 10h with 80% left (it used
		// its share early in the week).
		inv.Accounts[0] = trendAcct(claudeAcct("claude-a", 20, 10*time.Hour, 0), eachFor(ratio)*0.2, 72)
		inv.Accounts[3] = trendAcct(codexAcct("codex-a", 20, 10*time.Hour, 0), eachFor(ratio)*0.2, 72)
		return inv
	}
	svc, _, _ := newTestService(t, Config{}, fleet(0.3))
	a := mustAdvise(t, svc, Request{})
	o, s := by(a, opus), by(a, sol)
	if o.ReasoningEffort != "xhigh" || o.Mode != ModeFast || s.ReasoningEffort != "xhigh" || s.Mode != ModePriority || o.Posture != PostureUseMore {
		t.Fatalf("opus %+v\nsol %+v", o, s)
	}
	p := posture(a, "claude")
	if p.Trend.ExpiringSoonPercent == nil || *p.Trend.ExpiringSoonPercent < 50 || p.Trend.ExpiringWithinHours != 24 {
		t.Fatalf("claude trend = %+v", p.Trend)
	}
	if !strings.Contains(a.Text(), "use more, extra high + fast (") || !strings.Contains(a.Text(), "% of one account expires unused within 24h at this trend)") {
		t.Fatalf("text = %s", a.Text())
	}

	// The same expiring allowance at a normal pace adds nothing.
	svc, _, _ = newTestService(t, Config{}, fleet(0.8))
	a = mustAdvise(t, svc, Request{})
	if o := by(a, opus); o.ReasoningEffort != "high" || o.Mode != ModeStandard {
		t.Fatalf("normal pace: %+v", o)
	}

	// Close to a 5h limit, or with fast mode off: extra high without fast.
	inv := fleet(0.3)
	inv.Accounts[0].Windows[1].UsedPercent = 90
	svc, _, _ = newTestService(t, Config{}, inv)
	if o := by(mustAdvise(t, svc, Request{}), opus); o.ReasoningEffort != "xhigh" || o.Mode != ModeStandard {
		t.Fatalf("caution: %+v", o)
	}
	svc, _, _ = newTestService(t, Config{FastMode: "off"}, fleet(0.3))
	if s := by(mustAdvise(t, svc, Request{}), sol); s.ReasoningEffort != "xhigh" || s.Mode != ModeStandard {
		t.Fatalf("fast mode off: %+v", s)
	}
}

// Codex credits above the floors raise the daily budget while routing spends
// them, but never unlock priority on their own.
func TestCreditsRaiseBudgetButNeverPriority(t *testing.T) {
	fleet := func(enabled bool) Inventory {
		inv := trendFleet(eachFor(0.8), 8, 72)
		inv.Accounts[3].Credits = &Credits{Balance: 59855, Spendable: 52105, Floor: 7500, Enabled: enabled}
		inv.Accounts[4].Credits = &Credits{Balance: 61926, Spendable: 54426, Floor: 7500, Enabled: enabled}
		return inv
	}
	svc, _, _ := newTestService(t, Config{}, fleet(true))
	a := mustAdvise(t, svc, Request{})
	p := posture(a, "codex")
	// 106531 credits / 12800 per allowance = 416% of the two accounts as
	// one, over 7 days: 28.6 + 59.4 = 88%/day.
	if !near(p.Trend.CreditsPercent, 416, 0.5) || p.Trend.CreditSpendDays != 7 || !near(p.Trend.BudgetPercentPerDay, 200.0/7+416.13/7, 0.1) {
		t.Fatalf("credit budget = %+v", p.Trend)
	}
	s := by(a, sol)
	if p.Posture != PostureUseMore || s.ReasoningEffort != "xhigh" || s.Mode != ModeStandard {
		t.Fatalf("credits on: %+v %+v", p, s)
	}
	if !strings.Contains(a.Text(), "codex trend: last 3 days 16.0%/day of one account; even budget 88.0%/day (2 accounts x 100%/7 days + 416% credits over 7 days) -> 0.18 of budget: use more, extra high\n") {
		t.Fatalf("text = %s", a.Text())
	}
	// Without credit spending the budget is the weekly allowance alone.
	svc, _, _ = newTestService(t, Config{}, fleet(false))
	a = mustAdvise(t, svc, Request{})
	if p := posture(a, "codex"); p.Trend.CreditsPercent != 0 || !near(*p.Trend.Ratio, 16/(200.0/7), 0.01) || p.Posture != PostureNormal {
		t.Fatalf("credits off: %+v", p.Trend)
	}
	// The credit spreading period is configurable.
	svc, _, _ = newTestService(t, Config{CreditSpendDays: 14}, fleet(true))
	if p := posture(mustAdvise(t, svc, Request{}), "codex"); !near(p.Trend.BudgetPercentPerDay, 200.0/7+416.13/14, 0.1) {
		t.Fatalf("credit-spend-days 14: %+v", p.Trend)
	}
}

// Claude has no durable quota observations: its trend comes from the
// request ledger priced against the configured capacity.
func TestLedgerTrend(t *testing.T) {
	inv := trendFleet(0, eachFor(0.8), 72)
	for i := 0; i < 3; i++ {
		inv.Accounts[i].History = nil
		inv.Accounts[i].Usage = &UsageHistory{TrendFrom: t0.Add(-72 * time.Hour), TrendTo: t0}
	}
	// 12%/day of a 2000-unit account for 3 days = 720 units = 36M Opus
	// output tokens at 20 per million.
	inv.Accounts[0].Usage.Trend = []ModelUsage{{Model: opus, OutputTokens: 36_000_000}}
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	p := posture(a, "claude")
	if p.Trend.Basis != TrendLedger || p.Trend.PercentPerDay == nil || !near(*p.Trend.PercentPerDay, 12, 0.01) || p.Posture != PostureUseMore {
		t.Fatalf("ledger trend = %+v %+v", p, p.Trend)
	}
	// A ledger that covers only 10 hours leaves the trend unknown.
	for i := 0; i < 3; i++ {
		inv.Accounts[i].Usage.TrendFrom = t0.Add(-10 * time.Hour)
	}
	svc, _, _ = newTestService(t, Config{}, inv)
	a = mustAdvise(t, svc, Request{})
	if p := posture(a, "claude"); p.Trend.PercentPerDay != nil || p.Posture != PostureNormal || by(a, opus).ReasoningEffort != "high" {
		t.Fatalf("short ledger = %+v", p.Trend)
	}
	if !strings.Contains(a.Text(), "claude trend: unknown (history covers 10h; needs 24h); even budget 42.9%/day (3 accounts x 100%/7 days) -> normal, high\n") {
		t.Fatalf("text = %s", a.Text())
	}
}

func TestAllowanceUseAcrossResets(t *testing.T) {
	r1, r2 := t0.Add(time.Hour), t0.Add(169*time.Hour)
	samples := []Sample{
		{At: t0.Add(-48 * time.Hour), UsedPercent: 40, ResetAt: r1},
		{At: t0.Add(-40 * time.Hour), UsedPercent: 50, ResetAt: r1},
		{At: t0.Add(-39 * time.Hour), UsedPercent: 49, ResetAt: r1}, // provider rounding
		{At: t0.Add(-38 * time.Hour), UsedPercent: 52, ResetAt: r1},
		{At: t0.Add(-30 * time.Hour), UsedPercent: 5, ResetAt: r2}, // redeemed reset
		{At: t0.Add(-60 * time.Hour), UsedPercent: 1, ResetAt: r1}, // before the window
		{At: t0, UsedPercent: 25, ResetAt: r2},
	}
	used, covered := allowanceUse(samples, t0.Add(-50*time.Hour))
	if !near(used, 12+5+20, 1e-9) || covered != 48*time.Hour {
		t.Fatalf("used %v covered %v", used, covered)
	}
}

// Postures order normal and use more first, then conserve, then avoid.
func TestTextShowsPosturesAndTrends(t *testing.T) {
	inv := trendFleet(eachFor(1.2), eachFor(0.3), 72)
	svc, _, _ := newTestService(t, Config{}, inv)
	a := mustAdvise(t, svc, Request{})
	text := a.Text()
	t.Log("\n" + text)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	want := []string{
		"codex use more, claude conserve, opencode-go avoid | reuse until 2026-11-12T12:05:00Z (usage moves; ask again after the reuse window)",
		"codex use more: allowance would go unused at the recent pace; extra high is fine",
		"claude conserve: runs short before reset at the recent pace; use fewer subagents",
		"opencode-go avoid: budget unknown (no usage tracker configured)",
		"claude weekly, all 3 accounts as one: ",
		"claude accounts: ",
		"claude trend: last 3 days 51.4%/day of one account; even budget 42.9%/day (3 accounts x 100%/7 days) -> 1.20 of budget: conserve, high",
		"codex weekly, all 2 accounts as one: ",
		"codex accounts: ",
		"codex trend: last 3 days 8.6%/day of one account; even budget 28.6%/day (2 accounts x 100%/7 days) -> 0.30 of budget: use more, extra high",
		"gpt-6.1-sol effort=xhigh mode=standard | use more, extra high | codex: gpt-6.1-sol, opencode: gpt-6.1-sol",
		"claude-opus-5-5 effort=high mode=standard | conserve, high | claude-code: opus, opencode: claude-opus-5-5",
	}
	if len(lines) != len(want) || len(text) > 2000 {
		t.Fatalf("lines = %d, bytes = %d:\n%s", len(lines), len(text), text)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	if a.Summary != "codex use more, claude conserve, opencode-go avoid" {
		t.Fatalf("summary = %q", a.Summary)
	}
	if got := []string{a.Postures[0].Posture, a.Postures[1].Posture, a.Postures[2].Posture}; got[0] != PostureUseMore || got[1] != PostureConserve || got[2] != PostureAvoid {
		t.Fatalf("postures = %v", got)
	}
}

// An effort the registry does not advertise falls back to the normal step.
func TestUnsupportedStepFallsBackToNormal(t *testing.T) {
	inv := trendFleet(eachFor(0.3), eachFor(0.3), 72)
	inv.Models[2].Levels = []string{"low", "medium", "high"}
	svc, _, _ := newTestService(t, Config{}, inv)
	if s := by(mustAdvise(t, svc, Request{}), sol); s == nil || s.ReasoningEffort != "high" || s.Mode != ModeStandard {
		t.Fatalf("sol = %+v", s)
	}
}

// The budget counts the pool's accounts, so a fourth account raises it.
func TestBudgetFollowsAccountCount(t *testing.T) {
	inv := trendFleet(eachFor(0.8), eachFor(0.8), 72)
	inv.Accounts = append(inv.Accounts, trendAcct(claudeAcct("claude-d", 60, 100*time.Hour, 0), 0, 72))
	svc, _, _ := newTestService(t, Config{}, inv)
	p := posture(mustAdvise(t, svc, Request{}), "claude")
	if p.Trend.Accounts != 4 || !near(p.Trend.BudgetPercentPerDay, 400.0/7, 0.01) || !near(*p.Trend.Ratio, 0.6, 0.01) {
		t.Fatalf("four accounts = %+v", p.Trend)
	}
	// A disabled account counts toward neither.
	inv.Accounts[len(inv.Accounts)-1].Blocked = "disabled"
	svc, _, _ = newTestService(t, Config{}, inv)
	if p := posture(mustAdvise(t, svc, Request{}), "claude"); p.Trend.Accounts != 3 {
		t.Fatalf("disabled account counted: %+v", p.Trend)
	}
}
