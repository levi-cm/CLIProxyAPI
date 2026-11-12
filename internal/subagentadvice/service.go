package subagentadvice

import (
	"context"
	"math"
	"sync"
	"time"
)

const (
	sampleRetention   = 26 * time.Hour
	sampleMinSpacing  = 4 * time.Minute
	maxSamplesPerAcct = 400
	maxContextTokens  = 10_000_000
)

// Service computes subagent advice from live inventory. Its only state is the
// recent quota samples used to measure usage pace; it reserves nothing.
type Service struct {
	mu      sync.Mutex
	compute sync.Mutex // one inventory collection at a time bounds keyless load
	cfg     Config
	source  InventorySource
	now     func() time.Time
	samples map[string][]Sample
}

// New creates a service. cfg is resolved over the defaults.
func New(cfg Config, source InventorySource) *Service {
	return &Service{cfg: cfg.Resolve(), source: source, now: time.Now, samples: map[string][]Sample{}}
}

// SetConfig applies a reloaded configuration.
func (s *Service) SetConfig(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg.Resolve()
	s.mu.Unlock()
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// Config returns the resolved configuration.
func (s *Service) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Service) inventory(ctx context.Context) (Inventory, error) {
	if s.source == nil {
		return Inventory{}, errorf(503, "inventory_unavailable", "No account inventory is configured.")
	}
	inv, err := s.source.Inventory(ctx)
	if err != nil {
		return Inventory{}, errorf(503, "inventory_unavailable", "Account inventory is unavailable.")
	}
	return inv, nil
}

// recordSamplesLocked keeps spaced long-window observations for pace estimates.
func (s *Service) recordSamplesLocked(inv Inventory, now time.Time) {
	live := map[string]bool{}
	for _, a := range inv.Accounts {
		live[a.ID] = true
		w, ok := bindingLongWindow(a.Windows)
		if !ok || w.ObservedAt.IsZero() || math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) {
			continue
		}
		list := s.samples[a.ID]
		if n := len(list); n == 0 || w.ObservedAt.Sub(list[n-1].At) >= sampleMinSpacing || !sameWindow(w.ResetAt, list[n-1].ResetAt) {
			list = append(list, Sample{At: w.ObservedAt, UsedPercent: w.UsedPercent, ResetAt: w.ResetAt})
		}
		cut := 0
		for cut < len(list)-1 && (now.Sub(list[cut].At) > sampleRetention || len(list)-cut > maxSamplesPerAcct) {
			cut++
		}
		s.samples[a.ID] = list[cut:]
	}
	for id := range s.samples {
		if !live[id] {
			delete(s.samples, id)
		}
	}
}

func (s *Service) samplesCopyLocked() func(string) []Sample {
	snapshot := make(map[string][]Sample, len(s.samples))
	for id, list := range s.samples {
		snapshot[id] = append([]Sample(nil), list...)
	}
	return func(id string) []Sample { return snapshot[id] }
}

// prepare collects inventory, records samples and builds a forecaster.
func (s *Service) prepare(ctx context.Context) (*forecaster, Inventory, error) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if !cfg.Active() {
		return nil, Inventory{}, errorf(503, "subagent_advisor_disabled", "The subagent advisor is disabled.")
	}
	s.compute.Lock()
	inv, err := s.inventory(ctx)
	s.compute.Unlock()
	if err != nil {
		return nil, Inventory{}, err
	}
	// Requests only read samples; the background sampler records them.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	return newForecaster(s.cfg, now, inv, s.samplesCopyLocked()), inv, nil
}

// Sample records quota observations so usage pace is measurable between requests.
func (s *Service) Sample(ctx context.Context) error {
	s.compute.Lock()
	inv, err := s.inventory(WithBackgroundRefresh(ctx))
	s.compute.Unlock()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordSamplesLocked(inv, s.now().UTC())
	return nil
}

// Advise returns which models subagents should use now and how freely. It
// is read-only and deterministic for the same inventory, samples and clock.
func (s *Service) Advise(ctx context.Context, req Request) (Advice, error) {
	if req.ContextTokens < 0 || req.ContextTokens > maxContextTokens {
		return Advice{}, errorf(400, "invalid_request", "context_tokens must be 0-%d.", maxContextTokens)
	}
	f, inv, err := s.prepare(ctx)
	if err != nil {
		return Advice{}, err
	}
	d := decide(f, req)
	refresh, reason := refreshAfter(f, d)
	out := Advice{
		Schema:        Schema,
		SchemaVersion: SchemaVersion,
		Status:        d.status,
		Posture:       d.posture,
		GeneratedAt:   f.now,
		Models:        []ModelAdvice{},
		Summary:       d.summary,
		Warnings:      append([]string{}, d.warnings...),
		Freshness:     f.freshness(inv),
		Providers:     f.providerSummaries(),
	}
	for _, c := range d.choices {
		out.Models = append(out.Models, modelAdvice(c))
	}
	if !d.retryAfter.IsZero() && d.status == StatusUnavailable {
		seconds := int(math.Ceil(d.retryAfter.Sub(f.now).Seconds()))
		seconds = max(seconds, 1)
		out.RetryAfterSeconds = &seconds
	}
	out.RefreshAfterSeconds = int(refresh / time.Second)
	out.RefreshReason = reason
	out.ValidUntil = f.now.Add(refresh)
	return out, nil
}

func modelAdvice(c modelChoice) ModelAdvice {
	harnesses := make(map[string]string, len(c.offer.cap.Harnesses))
	for name, model := range c.offer.cap.Harnesses {
		if model == "" {
			model = c.offer.model
		}
		harnesses[name] = model
	}
	m := ModelAdvice{
		Capability:       c.offer.cap.Name,
		Provider:         c.offer.provider,
		Model:            c.offer.model,
		ReasoningEffort:  c.effort,
		Mode:             c.mode,
		Basis:            c.kind,
		Harnesses:        harnesses,
		CapabilitySource: c.offer.capabilitySource,
		Reason:           c.reason,
	}
	if c.budget >= 0 {
		tasks := int(math.Min(c.budget, math.MaxInt32))
		m.BudgetTasks = &tasks
	}
	if !c.until.IsZero() {
		until := c.until
		m.BudgetUntil = &until
	}
	return m
}

// Refresh intervals by situation, clamped to the configured bounds.
const (
	surplusRefresh = 15 * time.Minute
	cautionRefresh = 10 * time.Minute
)

// refreshAfter picks how long the advice stays good. Advice only changes when
// quota moves meaningfully or a known event happens, so comfortable pools are
// re-checked rarely and the next reset or recovery always ends the interval.
func refreshAfter(f *forecaster, d decision) (time.Duration, string) {
	minWait := time.Duration(f.cfg.MinRefreshSeconds) * time.Second
	maxWait := time.Duration(f.cfg.MaxRefreshSeconds) * time.Second
	wait, reason := maxWait, "quota is comfortable and nothing is due to change soon"
	consider := func(candidate time.Duration, why string) {
		if candidate < wait {
			wait, reason = candidate, why
		}
	}
	kinds := map[string]bool{}
	stale := false
	for _, c := range d.choices {
		kinds[c.kind] = true
		stale = stale || c.stale
	}
	if d.status == StatusUnavailable {
		reason = "nothing is available and no recovery time is known"
		if !d.retryAfter.IsZero() {
			reason = "nothing is available; the earliest recovery is further out"
			consider(d.retryAfter.Sub(f.now), "nothing is available until the earliest account recovers")
		}
	}
	if kinds[KindSurplus] {
		consider(surplusRefresh, "spending allowance that would otherwise expire; the budget shrinks as it is used")
	}
	// With nothing to run, only a recovery or reset can change the answer.
	tight := kinds[KindConserve]
	for _, p := range f.pools {
		tight = tight || (len(d.choices) > 0 && p.demandKnown && p.shortfallBase > 0)
	}
	if tight {
		consider(cautionRefresh, "capacity is tight before the next refill")
	}
	if stale {
		consider(cautionRefresh, "advice relies on stale quota observations")
	}
	if at, why := f.nextChange(); !at.IsZero() {
		consider(at.Sub(f.now), why)
	}
	return min(max(wait, minWait), maxWait), reason
}

// nextChange returns the earliest known future event that changes what can
// be advised: an account recovering, a closed short window reopening, or a
// metered allowance resetting.
func (f *forecaster) nextChange() (time.Time, string) {
	var at time.Time
	var why string
	consider := func(t time.Time, reason string) {
		if t.After(f.now) && (at.IsZero() || t.Before(at)) {
			at, why = t, reason
		}
	}
	for _, st := range f.accounts {
		if st.blocked != "" {
			consider(st.blockedUntil, "an account recovers from "+st.blocked)
		}
		for _, b := range st.acct.ModelBlocks {
			consider(b.Until, "a model cooldown ends")
		}
		if st.metered {
			consider(st.long.ResetAt, "an account's allowance resets")
		}
		for _, w := range st.acct.Windows {
			if (w.Long && w.Model == "") || !f.windowRelevant(w) {
				continue
			}
			if w.Full || w.UsedPercent >= f.cfg.ShortWindowBlockPercent {
				consider(w.ResetAt, "a "+w.Name+" window reopens")
			}
		}
	}
	return at, why
}

// Forecast returns the diagnostic forecast behind the advice.
func (s *Service) Forecast(ctx context.Context, contextTokens int) (Forecast, error) {
	if contextTokens < 0 || contextTokens > maxContextTokens {
		return Forecast{}, errorf(400, "invalid_request", "context_tokens must be 0-%d.", maxContextTokens)
	}
	f, inv, err := s.prepare(ctx)
	if err != nil {
		return Forecast{}, err
	}
	sch := newScheduler(f, contextTokens)
	out := Forecast{Schema: ForecastSchema, SchemaVersion: SchemaVersion, GeneratedAt: f.now, Providers: f.providerSummaries(), Freshness: f.freshness(inv), Accounts: []AccountForecast{}, Models: []ModelForecast{}}
	for _, st := range f.accounts {
		out.Accounts = append(out.Accounts, f.accountView(st))
	}
	for _, o := range sch.offers {
		eligible := 0
		for _, st := range o.accounts {
			if f.eligible(st, o.model).ok {
				eligible++
			}
		}
		out.Models = append(out.Models, ModelForecast{Capability: o.cap.Name, Provider: o.provider, Model: o.model, State: o.state, Reason: o.reason, Efforts: nonNil(o.efforts), Modes: nonNil(o.modes), CapabilitySource: o.capabilitySource, EligibleAccounts: eligible})
	}
	return out, nil
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
