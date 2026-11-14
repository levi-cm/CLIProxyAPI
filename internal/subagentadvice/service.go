package subagentadvice

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/fleetactivity"
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
	activity *fleetactivity.Tracker
	openCode openCodeReading
	client   *http.Client
	fetching sync.Mutex // one tracker fetch at a time
	// perAgent remembers each provider's last measured per-agent rate, the
	// fallback while the fleet is idle.
	perAgent map[string]measuredRate
}

type measuredRate struct {
	rate float64
	at   time.Time
}

// perAgentMemory is how long a measured per-agent rate stays a fallback.
const perAgentMemory = 24 * time.Hour

// New creates a service. cfg is resolved over the defaults. Fleet activity
// comes from the process-wide tracker.
func New(cfg Config, source InventorySource) *Service {
	return &Service{cfg: cfg.Resolve(), source: source, now: time.Now, samples: map[string][]Sample{}, activity: fleetactivity.Default(), client: &http.Client{}, perAgent: map[string]measuredRate{}}
}

// SetActivity replaces the fleet activity tracker (tests).
func (s *Service) SetActivity(t *fleetactivity.Tracker) {
	s.mu.Lock()
	s.activity = t
	s.mu.Unlock()
}

// SetHTTPClient replaces the client used for the OpenCode Go tracker (tests).
func (s *Service) SetHTTPClient(c *http.Client) {
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
}

// SampleOpenCode refreshes the OpenCode Go reading from the usage tracker. It
// returns at once when a fetch is already running or no URL is configured.
func (s *Service) SampleOpenCode(ctx context.Context) error {
	s.mu.Lock()
	url, client := s.cfg.OpenCodeUsageURL, s.client
	s.mu.Unlock()
	if url == "" || !s.fetching.TryLock() {
		return nil
	}
	defer s.fetching.Unlock()
	s.mu.Lock()
	now := s.now().UTC()
	s.mu.Unlock()
	reading, err := fetchOpenCode(ctx, client, url, now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.openCode = reading
	s.mu.Unlock()
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

func (s *Service) lastPerAgent(now time.Time) map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]float64{}
	for provider, m := range s.perAgent {
		if now.Sub(m.at) <= perAgentMemory {
			out[provider] = m.rate
		}
	}
	return out
}

func (s *Service) rememberPerAgent(d decision, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for provider, r := range d.rates {
		if r != nil && r.perAgentBasis == PerAgentMeasured {
			s.perAgent[provider] = measuredRate{rate: r.perAgent, at: now}
		}
	}
}

// fleetState returns the current fleet activity and OpenCode Go usage.
func (s *Service) fleetState(f *forecaster) (fleetactivity.Snapshot, *OpenCodeUsage) {
	s.mu.Lock()
	activity, reading := s.activity, s.openCode
	s.mu.Unlock()
	return activity.Snapshot(time.Duration(f.cfg.AgentWindowSeconds) * time.Second), openCodeUsage(f.cfg, reading, f.now)
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
	activity, openCode := s.fleetState(f)
	d := decide(f, req, activity, openCode, s.lastPerAgent(f.now))
	s.rememberPerAgent(d, f.now)
	refresh, reason := refreshAfter(f, d)
	out := Advice{
		Schema:        Schema,
		SchemaVersion: SchemaVersion,
		Status:        d.status,
		Posture:       d.posture,
		RoomAgents:    d.room,
		Fleet:         d.activity,
		GeneratedAt:   f.now,
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
		ReasoningEffort:  c.effort(),
		Mode:             c.mode(),
		Basis:            c.kind,
		Step:             c.step + 1,
		Steps:            len(c.offer.ladder),
		Harnesses:        harnesses,
		CapabilitySource: c.offer.capabilitySource,
		Reason:           c.reason,
	}
	if c.room >= 0 && c.kind != KindUnmetered {
		room := c.room
		m.RoomAgents = &room
	}
	return m
}

// providerSummaries adds each pool's fleet rate and the OpenCode Go usage to
// the forecaster's provider summaries.
func (d decision) providerSummaries(f *forecaster) []ProviderSummary {
	out := f.providerSummaries()
	for i := range out {
		if r := d.rates[out[i].Provider]; r != nil {
			out[i].Fleet = r.view()
		}
		if out[i].Provider == OpenCodeProvider {
			out[i].OpenCode = d.openCode
		}
	}
	return out
}

func (r *fleetRate) view() *FleetRate {
	return &FleetRate{
		State: r.state, Accounts: r.accounts,
		NeedPercentPerHour: round(r.need, 3), CreditsPercentPerHour: round(r.needCredits, 3), TargetPercentPerHour: round(r.target, 3),
		RecentPercentPerHour: round(r.recent, 3), RecentBasis: r.recentBasis, Agents: r.agents,
		PerAgentPercentPerHour: round(r.perAgent, 3), PerAgentBasis: r.perAgentBasis, RoomAgents: r.room,
		ExpiringSoonPercent: round(r.expiring, 1), TimeSensitive: r.timeSensitive,
	}
}

// refreshAfter is the reuse window: advice describes a shared, moving fleet,
// so every orchestrator asks again after reuse-seconds, sooner when a known
// event (a reset or recovery) changes the answer first.
func refreshAfter(f *forecaster, d decision) (time.Duration, string) {
	minWait := time.Duration(f.cfg.MinRefreshSeconds) * time.Second
	maxWait := time.Duration(f.cfg.MaxRefreshSeconds) * time.Second
	wait, reason := time.Duration(f.cfg.ReuseSeconds)*time.Second, "the fleet moves; ask again after the reuse window"
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
