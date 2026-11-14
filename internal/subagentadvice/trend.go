package subagentadvice

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// trendMinCoverage is the least history a trend needs; a few hours say
// nothing about a weekly pace.
const trendMinCoverage = 24 * time.Hour

// Trend bases.
const (
	TrendAllowance = "allowance_history"
	TrendLedger    = "request_ledger"
	TrendMixed     = "mixed"
)

// poolTrend is a provider pool's average daily use over the trend window
// against an even daily budget, in percent of one account.
type poolTrend struct {
	days        float64
	covered     time.Duration
	basis       string
	known       bool
	unknown     string
	perDay      float64
	accounts    int
	periodDays  float64
	allowance   float64
	credits     float64
	creditDays  float64
	budget      float64
	expiring    float64
	hasExpiring bool
	horizon     float64
}

func (t *poolTrend) ratio() float64 {
	if t.budget <= 0 {
		return math.Inf(1)
	}
	return t.perDay / t.budget
}

func (t *poolTrend) view() *Trend {
	v := &Trend{
		Days: t.days, CoveredDays: round(t.covered.Hours()/24, 2), Basis: t.basis, Unknown: t.unknown,
		Accounts: t.accounts, PeriodDays: round(t.periodDays, 2), AllowanceBudgetPercentPerDay: round(t.allowance, 2),
		CreditsPercent: round(t.credits, 0), BudgetPercentPerDay: round(t.budget, 2),
	}
	if t.credits > 0 {
		v.CreditSpendDays = t.creditDays
	}
	if t.known {
		perDay, ratio := round(t.perDay, 2), round(t.ratio(), 2)
		v.PercentPerDay, v.Ratio = &perDay, &ratio
		if t.hasExpiring {
			expiring := round(t.expiring, 1)
			v.ExpiringSoonPercent, v.ExpiringWithinHours = &expiring, t.horizon
		}
	}
	return v
}

// poolTrend sums each account's daily use over the trend window and sets the
// even budget: every account's long-window allowance spread evenly over its
// period, plus credits above the floors over credit-spend-days while routing
// spends them. Disabled accounts count toward neither.
func (f *forecaster) poolTrend(provider string) *poolTrend {
	cfg := f.cfg
	t := &poolTrend{days: cfg.TrendDays, creditDays: cfg.CreditSpendDays, horizon: cfg.FastHorizonHours, known: true}
	var accounts []*accountState
	ref := 0.0
	for _, st := range f.accounts {
		if st.acct.Provider == provider && st.blocked != "disabled" {
			accounts = append(accounts, st)
			ref += st.capacity
		}
	}
	if len(accounts) == 0 {
		t.known, t.unknown = false, "no accounts"
		return t
	}
	ref /= float64(len(accounts))
	weight := func(st *accountState) float64 { return st.capacity / ref }
	bases := map[string]bool{}
	periods := map[float64]bool{}
	for _, st := range accounts {
		t.accounts++
		period := defaultLongWindow.Hours() / 24
		if st.metered && st.long.DurationSeconds > 0 {
			period = float64(st.long.DurationSeconds) / 86400
		}
		periods[period] = true
		t.allowance += 100 / period * weight(st)
		if c := st.acct.Credits; c != nil && c.Enabled && st.creditUnits > 0 {
			t.credits += st.creditUnits / ref * 100
		}
		perDay, covered, basis, why := f.accountTrend(st)
		if t.accounts == 1 || covered < t.covered {
			t.covered = covered
		}
		if why != "" {
			if t.unknown == "" {
				t.unknown = why
			}
			t.known = false
			continue
		}
		bases[basis] = true
		t.perDay += perDay * weight(st)
	}
	for p := range periods {
		t.periodDays = p
	}
	if len(periods) > 1 {
		t.periodDays = 0 // mixed weekly and monthly windows
	}
	t.budget = t.allowance + t.credits/cfg.CreditSpendDays
	if !t.known {
		t.perDay = 0
		return t
	}
	for b := range bases {
		t.basis = b
	}
	if len(bases) > 1 {
		t.basis = TrendMixed
	}
	t.expiring, t.hasExpiring = f.expiringAtTrend(accounts, ref, t.perDay)
	return t
}

// accountTrend is one account's use per day (percent of its own allowance)
// over the trend window, from whichever source covers more of it: durable
// allowance observations, or request tokens priced against the configured
// capacity. why is set when neither covers a day.
func (f *forecaster) accountTrend(st *accountState) (float64, time.Duration, string, string) {
	from := f.now.Add(-time.Duration(f.cfg.TrendDays * 24 * float64(time.Hour)))
	var samples []Sample
	samples = append(samples, st.acct.History...)
	if f.samples != nil {
		samples = append(samples, f.samples(st.acct.ID)...)
	}
	if st.metered {
		samples = append(samples, Sample{At: st.long.ObservedAt, UsedPercent: st.used, ResetAt: st.long.ResetAt})
	}
	used, covered := allowanceUse(samples, from)
	basis := TrendAllowance
	if u := st.acct.Usage; st.capacityKnown && u != nil && !u.TrendFrom.IsZero() && u.TrendTo.After(u.TrendFrom) {
		start := u.TrendFrom
		if start.Before(from) {
			start = from
		}
		if span := u.TrendTo.Sub(start); span > covered {
			used, covered, basis = f.usageCost(st.acct.Provider, u.Trend)/st.capacity*100, span, TrendLedger
		}
	}
	switch {
	case covered <= 0:
		return 0, 0, "", "no usage history yet; needs " + roughDuration(trendMinCoverage)
	case covered < trendMinCoverage:
		return 0, covered, "", fmt.Sprintf("history covers %s; needs %s", roughDuration(covered), roughDuration(trendMinCoverage))
	}
	return used / covered.Hours() * 24, covered, basis, ""
}

// allowanceUse sums the allowance used across observations since from: the
// rise of each window's peak, plus what a new window shows at its first
// observation (it started empty). Allowance used between a window's last
// observation and its reset is missed, so the sum errs low.
func allowanceUse(samples []Sample, from time.Time) (float64, time.Duration) {
	var list []Sample
	for _, s := range samples {
		if s.At.IsZero() || s.At.Before(from) || math.IsNaN(s.UsedPercent) || math.IsInf(s.UsedPercent, 0) {
			continue
		}
		list = append(list, s)
	}
	if len(list) < 2 {
		return 0, 0
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	used, peak := 0.0, clamp(list[0].UsedPercent, 0, 100)
	window := list[0].ResetAt
	for _, s := range list[1:] {
		pct := clamp(s.UsedPercent, 0, 100)
		switch {
		case sameWindow(s.ResetAt, window):
			if pct > peak {
				used += pct - peak
				peak = pct
			}
		case s.ResetAt.After(window):
			// A refill or redeemed reset started a new window.
			used += pct
			peak, window = pct, s.ResetAt
		}
	}
	return used, list[len(list)-1].At.Sub(list[0].At)
}

// expiringAtTrend is the allowance (percent of one account) that would still
// expire unused within the fast horizon if the pool keeps its trend, with
// the router draining the earliest-resetting account first. Credits never
// count: they do not expire.
func (f *forecaster) expiringAtTrend(accounts []*accountState, ref, perDay float64) (float64, bool) {
	type allowance struct {
		hours   float64
		percent float64
	}
	var list []allowance
	for _, st := range accounts {
		if !st.metered || st.long.ResetAt.IsZero() || !countsTowardNeed(st) {
			continue
		}
		left := f.room(st) / ref * 100
		if left <= 0 {
			continue
		}
		list = append(list, allowance{hours: math.Max(0, st.long.ResetAt.Sub(f.now).Hours()), percent: left})
	}
	if len(list) == 0 {
		return 0, false
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].hours < list[j].hours })
	drain, used, expiring := perDay/24, 0.0, 0.0
	for _, a := range list {
		take := math.Min(a.percent, math.Max(0, drain*a.hours-used))
		used += take
		if a.hours <= f.cfg.FastHorizonHours {
			expiring += a.percent - take
		}
	}
	return expiring, true
}

// countsTowardNeed reports whether an account's allowance can still be used
// before it resets: usable now, or blocked only until before its reset.
func countsTowardNeed(st *accountState) bool {
	if st.blocked == "" {
		return true
	}
	return !st.blockedUntil.IsZero() && (st.long.ResetAt.IsZero() || st.blockedUntil.Before(st.long.ResetAt))
}

// room is the allowance an account can still use above its reserve.
func (f *forecaster) room(st *accountState) float64 {
	pool := f.pools[st.acct.Provider]
	return math.Max(0, st.remainingUnits-st.reserveUnits(f.cfg, pool != nil && pool.demandKnown))
}
