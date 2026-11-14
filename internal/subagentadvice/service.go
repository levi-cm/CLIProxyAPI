package subagentadvice

import (
	"context"
	"math"
	"net/http"
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
// recent quota samples used to measure usage pace and the last OpenCode Go
// tracker reading; it reserves nothing.
type Service struct {
	mu       sync.Mutex
	compute  sync.Mutex // one inventory collection at a time bounds keyless load
	cfg      Config
	source   InventorySource
	now      func() time.Time
	samples  map[string][]Sample
	openCode openCodeReading
	client   *http.Client
	// fetchStarted is when the running tracker fetch began; zero when idle.
	fetchStarted time.Time
}

// New creates a service. cfg is resolved over the defaults.
func New(cfg Config, source InventorySource) *Service {
	return &Service{cfg: cfg.Resolve(), source: source, now: time.Now, samples: map[string][]Sample{}, client: &http.Client{}}
}

// SetHTTPClient replaces the client used for the OpenCode Go tracker (tests).
func (s *Service) SetHTTPClient(c *http.Client) {
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
}

// SampleOpenCode refreshes the OpenCode Go reading from the usage tracker. It
// returns at once when no URL is configured or a fetch started less than a
// reading lifetime ago. A fetch that hangs longer no longer blocks new ones,
// so a stuck connection cannot freeze the tracker as unreachable; failures
// are never cached, and only a newer reading replaces the current one.
func (s *Service) SampleOpenCode(ctx context.Context) error {
	s.mu.Lock()
	url, client, now := s.cfg.OpenCodeUsageURL, s.client, s.now().UTC()
	if url == "" || (!s.fetchStarted.IsZero() && now.Sub(s.fetchStarted) < openCodeMaxAge) {
		s.mu.Unlock()
		return nil
	}
	s.fetchStarted = now
	s.mu.Unlock()
	reading, err := fetchOpenCode(ctx, client, url, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fetchStarted.Equal(now) {
		s.fetchStarted = time.Time{}
	}
	if err != nil {
		return err
	}
	if reading.at.After(s.openCode.at) {
		s.openCode = reading
	}
	return nil
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

// openCodeState returns the OpenCode Go usage at the forecaster's clock.
func (s *Service) openCodeState(f *forecaster) *OpenCodeUsage {
	s.mu.Lock()
	reading := s.openCode
	s.mu.Unlock()
	return openCodeUsage(f.cfg, reading, f.now)
}

// Sample records quota observations so usage pace is measurable between
// requests, and lets the inventory source read the trend window's history.
func (s *Service) Sample(ctx context.Context) error {
	s.mu.Lock()
	span := time.Duration(s.cfg.TrendDays * 24 * float64(time.Hour))
	s.mu.Unlock()
	s.compute.Lock()
	inv, err := s.inventory(WithTrendSpan(WithBackgroundRefresh(ctx), span))
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
	d := decide(f, req, s.openCodeState(f))
	refresh, reason := refreshAfter(f, d)
	out := Advice{
		Schema:        Schema,
		SchemaVersion: SchemaVersion,
		Status:        d.status,
		GeneratedAt:   f.now,
		Postures:      append([]ProviderPosture{}, d.postures...),
		Models:        []ModelAdvice{},
		Summary:       d.summary,
		Warnings:      append([]string{}, d.warnings...),
		Freshness:     f.freshness(inv),
		Providers:     d.providerSummaries(f),
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
		ReasoningEffort:  c.step.Effort,
		Mode:             c.step.Mode,
		Posture:          c.posture,
		Harnesses:        harnesses,
		CapabilitySource: c.offer.capabilitySource,
		Reason:           c.reason,
	}
	return m
}

// providerSummaries adds the OpenCode Go usage to the forecaster's provider
// summaries.
func (d decision) providerSummaries(f *forecaster) []ProviderSummary {
	out := f.providerSummaries()
	for i := range out {
		if out[i].Provider == OpenCodeProvider {
			out[i].OpenCode = d.openCode
		}
	}
	return out
}

// refreshAfter is the reuse window: usage moves, so every orchestrator asks
// again after reuse-seconds, sooner when a known event (a reset or recovery)
// changes the answer first.
func refreshAfter(f *forecaster, d decision) (time.Duration, string) {
	minWait := time.Duration(f.cfg.MinRefreshSeconds) * time.Second
	maxWait := time.Duration(f.cfg.MaxRefreshSeconds) * time.Second
	wait, reason := time.Duration(f.cfg.ReuseSeconds)*time.Second, "usage moves; ask again after the reuse window"
	consider := func(candidate time.Duration, why string) {
		if candidate < wait {
			wait, reason = candidate, why
		}
	}
	if d.status == StatusUnavailable && !d.retryAfter.IsZero() {
		consider(d.retryAfter.Sub(f.now), "nothing is available until the earliest account recovers")
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
