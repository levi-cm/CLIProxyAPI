package subagentadvice

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	defaultLongWindow = 7 * 24 * time.Hour
	paceMinSpan       = time.Hour
	paceLongSpan      = 24 * time.Hour
	paceShortSpan     = 3 * time.Hour
	resetMatchSlack   = 2 * time.Minute
	// simHorizon covers every account's current weekly window and the refill
	// after it, so allowance that expires unused anywhere in the pool counts.
	simHorizon = 8 * 24 * time.Hour
	// pctUnits is the capacity assumed for an account whose capacity is
	// unknown, so one unit equals one percentage point of its window.
	pctUnits = 100.0
)

// Pace bases, from most to least reliable.
const (
	PaceObserved = "observed"
	PaceLedger   = "ledger"
	PaceUnknown  = "unknown"
)

// accountState is the forecast for one account.
type accountState struct {
	acct *Account
	ref  string

	capacity      float64 // units used for arithmetic (pctUnits when unknown)
	capacityKnown bool
	capacityBasis string

	metered     bool
	meterReason string
	long        Window
	used        float64
	age         time.Duration
	stale       bool

	pace           float64 // percentage points per hour
	paceKnown      bool
	paceBasis      string
	paceConfidence float64

	inFlightUnits  float64
	remainingUnits float64

	blocked      string
	blockedUntil time.Time
	// simAvailableAt is when the account can serve pool demand again.
	simAvailableAt time.Time
	simExcluded    bool

	wasteUnits   float64
	surplusUnits float64
}

func (st *accountState) reserveUnits(cfg Config, demandKnown bool) float64 {
	reserve := st.acct.ReservePercent
	if !demandKnown && cfg.ReservePercent > reserve {
		reserve = cfg.ReservePercent
	}
	return reserve / 100 * st.capacity
}

// poolState aggregates the metered accounts of one provider.
type poolState struct {
	provider      string
	accounts      []*accountState
	demand        float64 // units per hour, already multiplied by the safety factor
	demandKnown   bool
	confidence    float64
	horizon       time.Time
	shortfallBase float64
	nextReset     time.Time
	// wasteUnits is the allowance projected to expire unused at resets within
	// the surplus horizon, across all accounts and refills.
	wasteUnits float64
	// surplusUnits is wasteUnits discounted by pace confidence.
	surplusUnits float64
}

type forecaster struct {
	cfg     Config
	now     time.Time
	inv     Inventory
	samples func(id string) []Sample

	accounts []*accountState
	byID     map[string]*accountState
	pools    map[string]*poolState
}

func accountRef(provider, id string) string {
	h := sha256.Sum256([]byte(provider + "\x00" + id))
	return "acct_" + hex.EncodeToString(h[:6])
}

func newForecaster(cfg Config, now time.Time, inv Inventory, samples func(string) []Sample) *forecaster {
	f := &forecaster{cfg: cfg, now: now, inv: inv, samples: samples, byID: map[string]*accountState{}, pools: map[string]*poolState{}}
	accounts := make([]*Account, 0, len(inv.Accounts))
	for i := range inv.Accounts {
		a := &inv.Accounts[i]
		a.Provider = normalizeProvider(a.Provider)
		if strings.TrimSpace(a.ID) == "" {
			continue
		}
		if p, ok := cfg.Providers[a.Provider]; ok && p.Disabled {
			continue
		}
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Provider != accounts[j].Provider {
			return accounts[i].Provider < accounts[j].Provider
		}
		return accounts[i].ID < accounts[j].ID
	})
	// A pool must use one unit. Accounts of unknown capacity in a pool with
	// known capacities assume the smallest known one (conservative).
	poolMin := map[string]float64{}
	for _, a := range accounts {
		if units, _ := f.configuredCapacity(a); units > 0 && (poolMin[a.Provider] == 0 || units < poolMin[a.Provider]) {
			poolMin[a.Provider] = units
		}
	}
	for _, a := range accounts {
		st := f.account(a, poolMin[a.Provider])
		f.accounts = append(f.accounts, st)
		f.byID[a.ID] = st
	}
	f.buildPools()
	return f
}

// configuredCapacity returns the account's configured capacity and its basis.
func (f *forecaster) configuredCapacity(a *Account) (float64, string) {
	provider := f.cfg.Providers[a.Provider]
	plan := strings.ToLower(strings.TrimSpace(a.Plan))
	switch {
	case f.cfg.AccountCapacityUnits[a.ID] > 0:
		return f.cfg.AccountCapacityUnits[a.ID], "account"
	case provider.PlanCapacityUnits[plan] > 0:
		return provider.PlanCapacityUnits[plan], "plan"
	case provider.CapacityUnits > 0:
		return provider.CapacityUnits, "configured"
	}
	return 0, "unknown"
}

func (f *forecaster) account(a *Account, poolMin float64) *accountState {
	cfg := f.cfg
	st := &accountState{acct: a, ref: accountRef(a.Provider, a.ID), capacity: pctUnits, capacityBasis: "unknown", paceBasis: PaceUnknown}
	if units, basis := f.configuredCapacity(a); units > 0 {
		st.capacity, st.capacityKnown, st.capacityBasis = units, true, basis
	} else if poolMin > 0 {
		// Still unknown for pricing: costs stay percentage-based.
		st.capacity, st.capacityBasis = poolMin, "pool_minimum"
	}
	st.blocked, st.blockedUntil = a.Blocked, a.BlockedUntil

	long, found := bindingLongWindow(a.Windows)
	switch {
	case !found || long.ObservedAt.IsZero():
		st.meterReason = "no_quota_telemetry"
	case math.IsNaN(long.UsedPercent) || math.IsInf(long.UsedPercent, 0):
		st.meterReason = "invalid_observation"
	case f.now.Sub(long.ObservedAt) > time.Duration(cfg.ExpiredAfterSeconds)*time.Second:
		st.meterReason = "observation_expired"
	case !long.ResetAt.IsZero() && !long.ResetAt.After(f.now):
		// A new window began; its usage is unknown until the next observation.
		st.meterReason = "window_rolled_over"
	default:
		st.metered = true
		st.long = long
		st.used = clamp(long.UsedPercent, 0, 100)
		st.age = f.now.Sub(long.ObservedAt)
		if st.age < 0 {
			st.age = 0
		}
		st.stale = st.age > time.Duration(cfg.StaleAfterSeconds)*time.Second
	}
	if st.metered && (st.long.Full || st.used >= 100) && !a.ServingOnCredits && st.blocked == "" {
		st.blocked, st.blockedUntil = "allowance_exhausted", st.long.ResetAt
	}
	f.estimatePace(st)

	st.inFlightUnits = float64(a.ActiveRequests) * cfg.InFlightRequestPercent / 100 * st.capacity
	if st.metered {
		remaining := 100 - st.used
		if st.stale && st.paceKnown {
			// Usage only grows within a window, so a stale reading overstates
			// what remains; assume the account kept its pace since then.
			remaining -= st.pace * st.age.Hours()
		}
		if a.ServingOnCredits {
			remaining = 0
		}
		st.remainingUnits = math.Max(0, remaining/100*st.capacity-st.inFlightUnits)
	}

	// Pool simulation availability.
	switch {
	case st.blocked == "":
	case !st.blockedUntil.IsZero() && st.blockedUntil.After(f.now):
		st.simAvailableAt = st.blockedUntil
	default:
		st.simExcluded = true
	}
	if until, full := f.allModelShortWindowFull(a); full {
		if until.IsZero() {
			st.simExcluded = true
		} else if until.After(st.simAvailableAt) {
			st.simAvailableAt = until
		}
	}
	return st
}

// bindingLongWindow returns the all-model allowance window (weekly, monthly)
// with the least left, using the latest observation of each window.
func bindingLongWindow(windows []Window) (Window, bool) {
	latest := map[string]Window{}
	var names []string
	for _, w := range windows {
		if !w.Long || w.Model != "" {
			continue
		}
		prev, seen := latest[w.Name]
		if !seen {
			names = append(names, w.Name)
		}
		if !seen || w.ObservedAt.After(prev.ObservedAt) {
			latest[w.Name] = w
		}
	}
	var best Window
	found := false
	for _, name := range names {
		w := latest[name]
		used := w.UsedPercent
		if w.Full {
			used = 100
		}
		bestUsed := best.UsedPercent
		if best.Full {
			bestUsed = 100
		}
		if !found || used > bestUsed || (used == bestUsed && w.ResetAt.Before(best.ResetAt)) {
			best, found = w, true
		}
	}
	return best, found
}

// estimatePace derives the account's own consumption rate.
func (f *forecaster) estimatePace(st *accountState) {
	if st.metered {
		var samples []Sample
		samples = append(samples, st.acct.History...)
		if f.samples != nil {
			samples = append(samples, f.samples(st.acct.ID)...)
		}
		current := Sample{At: st.long.ObservedAt, UsedPercent: st.used, ResetAt: st.long.ResetAt}
		long, longSpan, okLong := paceFrom(samples, current, f.now, paceLongSpan)
		short, _, okShort := paceFrom(samples, current, f.now, paceShortSpan)
		if okLong || okShort {
			st.pace = math.Max(long, short)
			st.paceKnown, st.paceBasis = true, PaceObserved
			st.paceConfidence = 0.6
			if longSpan >= 6*time.Hour {
				st.paceConfidence = 0.85
			}
		}
	}
	if !st.paceKnown && st.capacityKnown && st.acct.Usage != nil {
		perHour24 := f.usageCost(st.acct.Provider, st.acct.Usage.Last24h) / 24
		perHour3 := f.usageCost(st.acct.Provider, st.acct.Usage.Last3h) / 3
		st.pace = math.Max(perHour24, perHour3) / st.capacity * 100
		st.paceKnown, st.paceBasis, st.paceConfidence = true, PaceLedger, 0.5
	}
	if st.paceKnown && st.stale {
		st.paceConfidence *= 0.5
	}
}

// paceFrom measures percentage points per hour between the oldest sample of
// the current window within span and the current observation. It never
// extends the measurement to now: an observation that has not been refreshed
// says nothing about the time since, so idle time counts only when observed.
func paceFrom(samples []Sample, current Sample, now time.Time, span time.Duration) (float64, time.Duration, bool) {
	start := now.Add(-span)
	var oldest *Sample
	for i := range samples {
		s := samples[i]
		if s.At.Before(start) || !s.At.Before(current.At) || !sameWindow(s.ResetAt, current.ResetAt) || math.IsNaN(s.UsedPercent) || math.IsInf(s.UsedPercent, 0) || s.UsedPercent > current.UsedPercent+0.5 {
			continue
		}
		if oldest == nil || s.At.Before(oldest.At) {
			oldest = &samples[i]
		}
	}
	if oldest == nil {
		return 0, 0, false
	}
	measured := current.At.Sub(oldest.At)
	if measured < paceMinSpan {
		return 0, measured, false
	}
	return math.Max(0, current.UsedPercent-oldest.UsedPercent) / measured.Hours(), measured, true
}

func sameWindow(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() && b.IsZero()
	}
	d := a.Sub(b)
	return d <= resetMatchSlack && d >= -resetMatchSlack
}

// usageCost prices ledger usage. Models without a price contribute nothing,
// which is acceptable because only capacity-known providers use this path and
// they configure a provider default price.
func (f *forecaster) usageCost(provider string, usage []ModelUsage) float64 {
	total := 0.0
	for _, u := range usage {
		price, ok := f.cfg.priceFor(provider, u.Model)
		if !ok {
			continue
		}
		total += price.cost(u.InputTokens, u.OutputTokens, u.CacheReadTokens)
	}
	return total
}

// allModelShortWindowFull reports whether a window that applies to every model
// is closed now, and when it reopens.
func (f *forecaster) allModelShortWindowFull(a *Account) (time.Time, bool) {
	for _, w := range a.Windows {
		if w.Long || w.Model != "" {
			continue
		}
		if f.windowClosed(w) {
			return w.ResetAt, true
		}
	}
	return time.Time{}, false
}

func (f *forecaster) windowRelevant(w Window) bool {
	if w.ObservedAt.IsZero() || f.now.Sub(w.ObservedAt) > time.Duration(f.cfg.ExpiredAfterSeconds)*time.Second {
		return false
	}
	return w.ResetAt.IsZero() || w.ResetAt.After(f.now)
}

func (f *forecaster) windowClosed(w Window) bool {
	return f.windowRelevant(w) && (w.Full || w.UsedPercent >= f.cfg.ShortWindowBlockPercent)
}

// eligibility describes whether an account may serve a model now.
type eligibility struct {
	ok      bool
	caution bool
	reason  string
	until   time.Time
}

func (f *forecaster) eligible(st *accountState, model string) eligibility {
	a := st.acct
	if st.blocked != "" {
		return eligibility{reason: st.blocked, until: st.blockedUntil}
	}
	if !contains(a.Models, model) {
		return eligibility{reason: "model_not_served"}
	}
	if b, ok := a.ModelBlocks[model]; ok && (b.Until.IsZero() || b.Until.After(f.now)) {
		return eligibility{reason: b.Reason, until: b.Until}
	}
	out := eligibility{ok: true}
	for _, w := range a.Windows {
		scoped := w.Model != ""
		if w.Long && !scoped {
			continue // the allowance window is handled by the forecast
		}
		if scoped && !matchModel([]string{w.Model}, model) {
			continue
		}
		if !f.windowRelevant(w) {
			continue
		}
		if !scoped && a.ServingOnCredits {
			// The policy authorized credits past the ordinary allowance windows;
			// cooldowns and model-scoped windows still block.
			continue
		}
		if w.Full || w.UsedPercent >= f.cfg.ShortWindowBlockPercent {
			return eligibility{reason: "window_full_" + w.Name, until: w.ResetAt}
		}
		if w.UsedPercent >= f.cfg.ShortWindowCautionPercent {
			out.caution = true
		}
	}
	return out
}

func (f *forecaster) buildPools() {
	for _, st := range f.accounts {
		if !st.metered {
			continue
		}
		p := f.pools[st.acct.Provider]
		if p == nil {
			p = &poolState{provider: st.acct.Provider, demandKnown: true, confidence: 1}
			f.pools[st.acct.Provider] = p
		}
		p.accounts = append(p.accounts, st)
	}
	for _, p := range f.pools {
		p.horizon = f.now.Add(simHorizon)
		for _, st := range p.accounts {
			if st.simExcluded {
				continue
			}
			if !st.paceKnown {
				p.demandKnown = false
				p.confidence = 0
			} else {
				p.demand += st.pace / 100 * st.capacity * f.cfg.SafetyFactor
				p.confidence = math.Min(p.confidence, st.paceConfidence)
			}
			if !st.long.ResetAt.IsZero() && (p.nextReset.IsZero() || st.long.ResetAt.Before(p.nextReset)) {
				p.nextReset = st.long.ResetAt
			}
		}
		if !p.demandKnown {
			continue
		}
		sim := f.simulate(p, nil)
		p.shortfallBase = sim.shortfall
		p.wasteUnits = sim.waste
		p.surplusUnits = math.Max(0, sim.waste*p.confidence)
		for _, st := range p.accounts {
			st.wasteUnits = sim.firstWaste[st.acct.ID]
			if st.blocked != "" || st.acct.ServingOnCredits || st.long.ResetAt.IsZero() {
				continue
			}
			st.surplusUnits = math.Max(0, st.wasteUnits*p.confidence)
		}
	}
}

// simResult is one pool simulation.
type simResult struct {
	// firstWaste is each account's unused allowance at its current reset.
	firstWaste map[string]float64
	// waste is all allowance left unused at resets within the surplus horizon,
	// including refilled windows.
	waste     float64
	shortfall float64
}

// simulate drains pool demand from the earliest-resetting available account,
// the order the deadline router uses, and reports the allowance left unused
// at resets plus the demand that could not be served.
func (f *forecaster) simulate(p *poolState, extra map[string]float64) simResult {
	type simAccount struct {
		id          string
		r, capacity float64
		reset       time.Time
		period      time.Duration
		availableAt time.Time
		wasted      bool
	}
	accts := make([]*simAccount, 0, len(p.accounts))
	for _, st := range p.accounts {
		if st.simExcluded {
			continue
		}
		period := time.Duration(st.long.DurationSeconds) * time.Second
		if period <= 0 {
			period = defaultLongWindow
		}
		r := st.remainingUnits - extra[st.acct.ID]
		accts = append(accts, &simAccount{id: st.acct.ID, r: r, capacity: st.capacity, reset: st.long.ResetAt, period: period, availableAt: st.simAvailableAt})
	}
	waste := map[string]float64{}
	total := 0.0
	surplusHorizon := f.now.Add(time.Duration(f.cfg.SurplusHorizonHours * float64(time.Hour)))
	shortfall := 0.0
	for _, a := range accts {
		if a.r < 0 {
			// Spending beyond what remains is unserved demand.
			shortfall += -a.r
			a.r = 0
		}
	}
	t := f.now
	for i := 0; t.Before(p.horizon) && i < 1000; i++ {
		next := p.horizon
		for _, a := range accts {
			if !a.reset.IsZero() && a.reset.After(t) && a.reset.Before(next) {
				next = a.reset
			}
			if a.availableAt.After(t) && a.availableAt.Before(next) {
				next = a.availableAt
			}
		}
		need := p.demand * next.Sub(t).Hours()
		order := make([]*simAccount, 0, len(accts))
		for _, a := range accts {
			if !a.availableAt.After(t) {
				order = append(order, a)
			}
		}
		sort.Slice(order, func(i, j int) bool {
			ri, rj := order[i].reset, order[j].reset
			if ri.IsZero() != rj.IsZero() {
				return !ri.IsZero()
			}
			if !ri.Equal(rj) {
				return ri.Before(rj)
			}
			return order[i].id < order[j].id
		})
		for _, a := range order {
			if need <= 0 {
				break
			}
			take := math.Min(a.r, need)
			a.r -= take
			need -= take
		}
		if need > 0 {
			shortfall += need
		}
		t = next
		for _, a := range accts {
			if !a.reset.IsZero() && !a.reset.After(t) {
				if !a.wasted {
					waste[a.id] = a.r
					a.wasted = true
				}
				if !a.reset.After(surplusHorizon) {
					total += a.r
				}
				a.r = a.capacity
				a.reset = a.reset.Add(a.period)
			}
		}
	}
	return simResult{firstWaste: waste, waste: total, shortfall: shortfall}
}

// fits reports whether extra spend keeps the pool able to serve forecast
// demand until resets, and respects operator reserves.
func (f *forecaster) fits(provider string, extra map[string]float64) bool {
	p := f.pools[provider]
	if p == nil {
		return true
	}
	for _, st := range p.accounts {
		spend := extra[st.acct.ID]
		if spend <= 0 {
			continue
		}
		if st.remainingUnits-spend < st.reserveUnits(f.cfg, p.demandKnown) {
			return false
		}
	}
	if !p.demandKnown {
		return true
	}
	return f.simulate(p, extra).shortfall <= p.shortfallBase+1e-9
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Max(lo, math.Min(hi, v))
}
