package subagentadvice

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/fleetactivity"
)

// Offer states.
const (
	OfferMetered     = "metered"
	OfferUnmetered   = "unmetered"
	OfferUnavailable = "unavailable"
)

var effortCostMultiplier = map[string]float64{"none": 0.2, "minimal": 0.3, "low": 0.5, "medium": 0.75, "high": 1, "xhigh": 1.5, "max": 2.5}

type offer struct {
	cap      ModelCapability
	model    string
	provider string
	// ladder is the capability's ladder, limited to supported efforts.
	ladder           []Step
	efforts          []string
	modes            []string
	capabilitySource string
	accounts         []*accountState
	state            string
	reason           string
	nextAvailable    time.Time
	override         bool
}

// Postures tell every orchestrator how the shared fleet stands. Room is
// shared by all orchestrators; it is advice, never a reservation or cap.
const (
	PostureRoom        = "room"
	PostureHold        = "hold"
	PostureConserve    = "conserve"
	PostureUnknown     = "unknown"
	PostureUnavailable = "unavailable"
)

// modelChoice is the advice for one capability.
type modelChoice struct {
	offer *offer
	kind  string
	step  int
	// room is the extra agents the provider can take; -1 when unknown.
	room   int
	stale  bool
	reason string
}

func (c modelChoice) effort() string { return c.offer.ladder[c.step].Effort }
func (c modelChoice) mode() string   { return c.offer.ladder[c.step].Mode }

// Per-agent rate bases.
const (
	PerAgentMeasured     = "measured"
	PerAgentLastMeasured = "last_measured"
	PerAgentTaskEstimate = "task_estimate"
)

// Fleet rate states.
const (
	RateRoom     = "room"
	RateHold     = "hold"
	RateConserve = "conserve"
)

// fleetRate is a provider pool's usage rate against the rate that would use
// every account's remaining allowance by its reset. Rates are percentage
// points of one account per hour.
type fleetRate struct {
	provider      string
	accounts      int
	needAllowance float64
	needCredits   float64
	need          float64
	target        float64
	recent        float64
	recentBasis   string
	agents        int
	perAgent      float64
	perAgentBasis string
	room          int
	state         string
	// expiring is allowance (percent of one account) that would still
	// expire unused within the fast horizon with the fleet at the target.
	expiring      float64
	timeSensitive bool
}

type decision struct {
	status     string
	posture    string
	choices    []modelChoice
	warnings   []string
	retryAfter time.Time
	summary    string
	rates      map[string]*fleetRate
	openCode   *OpenCodeUsage
	activity   fleetactivity.Snapshot
	room       int
}

type scheduler struct {
	f      *forecaster
	input  int
	offers []*offer
}

func newScheduler(f *forecaster, contextTokens int) *scheduler {
	s := &scheduler{f: f, input: f.cfg.TaskInputTokens}
	s.offers = f.resolveOffers(contextTokens)
	for _, o := range s.offers {
		s.classify(o)
	}
	return s
}

// routerOrder lists eligible accounts in the order the deadline router would
// prefer them: earliest reset first, unknown resets last.
func (s *scheduler) routerOrder(o *offer) []*accountState {
	var out []*accountState
	for _, st := range o.accounts {
		if s.f.eligible(st, o.model).ok {
			out = append(out, st)
		}
	}
	forced := s.f.inv.RoutingAccounts[o.provider]
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// An operator override sends requests to its account first.
		if forced != "" && (a.acct.ID == forced) != (b.acct.ID == forced) {
			return a.acct.ID == forced
		}
		if a.metered != b.metered {
			return a.metered
		}
		ra, rb := a.long.ResetAt, b.long.ResetAt
		if ra.IsZero() != rb.IsZero() {
			return !ra.IsZero()
		}
		if !ra.Equal(rb) {
			return ra.Before(rb)
		}
		return a.ref < b.ref
	})
	return out
}

// classify sets each offer's state from current evidence.
func (s *scheduler) classify(o *offer) {
	if o.reason != "" && o.state == OfferUnavailable {
		return
	}
	eligible := s.routerOrder(o)
	if len(eligible) == 0 {
		o.state = OfferUnavailable
		o.reason = "no_eligible_account"
		for _, st := range o.accounts {
			e := s.f.eligible(st, o.model)
			if o.reason == "no_eligible_account" && e.reason != "" {
				o.reason = e.reason
			}
			if !e.until.IsZero() && (o.nextAvailable.IsZero() || e.until.Before(o.nextAvailable)) {
				o.nextAvailable = e.until
			}
		}
		return
	}
	for _, st := range eligible {
		if st.metered {
			o.state, o.reason = OfferMetered, ""
			return
		}
	}
	o.state, o.reason = OfferUnmetered, "no_quota_telemetry"
}

// room is the spend an account can take without breaching its reserve.
func (s *scheduler) room(st *accountState) float64 {
	pool := s.f.pools[st.acct.Provider]
	return math.Max(0, st.remainingUnits-st.reserveUnits(s.f.cfg, pool != nil && pool.demandKnown))
}

// countsTowardNeed reports whether an account's allowance can still be used
// before it resets: usable now, or blocked only until before its reset.
func countsTowardNeed(st *accountState) bool {
	if st.blocked == "" {
		return true
	}
	return !st.blockedUntil.IsZero() && (st.long.ResetAt.IsZero() || st.blockedUntil.Before(st.long.ResetAt))
}

// taskRate turns the per-task cost estimate at the ladder's first step into
// a per-agent rate (percent of one account per hour).
func (s *scheduler) taskRate(o *offer, st *accountState) float64 {
	cfg := s.f.cfg
	hours := cfg.TaskMinutes / 60
	if o == nil || st == nil || len(o.ladder) == 0 {
		return cfg.UnmeteredSubagentPercent / hours
	}
	cost := s.f.slotCost(o, st, o.ladder[0].Effort, ModeStandard, s.input)
	return cost / st.capacity * 100 / hours
}

// fleetRate compares the pool's recent use with the rate that would use
// every account's remaining allowance (and credits above the floors) by its
// reset, and turns the gap into room for more agents.
func (s *scheduler) fleetRate(provider string, o *offer, agents int, lastPerAgent float64) *fleetRate {
	pool := s.f.pools[provider]
	if pool == nil || len(pool.accounts) == 0 {
		return nil
	}
	cfg := s.f.cfg
	ref := 0.0
	for _, st := range pool.accounts {
		ref += st.capacity
	}
	ref /= float64(len(pool.accounts))
	pct := func(units float64) float64 { return units / ref * 100 }
	r := &fleetRate{provider: provider, agents: agents}
	type allowance struct {
		reset   time.Time
		hours   float64
		percent float64
	}
	var list []allowance
	recentKnown, observed := false, false
	for _, st := range pool.accounts {
		if st.recentBasis != "" {
			r.recent += st.recent * st.capacity / ref
			recentKnown = true
			observed = observed || st.recentBasis == PaceObserved
		}
		if !countsTowardNeed(st) {
			continue
		}
		r.accounts++
		if st.spendsCredits() {
			// Credits do not expire; spread them over the surplus horizon.
			r.needCredits += pct(st.creditUnits) / cfg.SurplusHorizonHours
		}
		left := pct(s.room(st))
		if left <= 0 {
			continue
		}
		hours := cfg.SurplusHorizonHours
		if !st.long.ResetAt.IsZero() {
			hours = math.Max(1, st.long.ResetAt.Sub(s.f.now).Hours())
		}
		r.needAllowance += left / hours
		list = append(list, allowance{reset: st.long.ResetAt, hours: hours, percent: left})
	}
	r.need = r.needAllowance + r.needCredits
	r.target = r.need * cfg.TargetFraction

	// Allowance that expires unused soon even at the target: the router
	// drains the earliest-resetting account first.
	sort.SliceStable(list, func(i, j int) bool { return list[i].hours < list[j].hours })
	drain, used := r.needAllowance*cfg.TargetFraction, 0.0
	for _, a := range list {
		take := math.Min(a.percent, math.Max(0, drain*a.hours-used))
		used += take
		if !a.reset.IsZero() && a.hours <= cfg.FastHorizonHours {
			r.expiring += a.percent - take
		}
	}
	r.timeSensitive = r.expiring > cfg.FastMinPercent

	var first *accountState
	if o != nil {
		if order := s.routerOrder(o); len(order) > 0 {
			first = order[0]
		}
	}
	if first == nil {
		first = pool.accounts[0]
	}
	r.recentBasis = PaceLedger
	if observed {
		r.recentBasis = PaceObserved
	}
	switch {
	case recentKnown && r.recent > 0:
		r.perAgent, r.perAgentBasis = r.recent/float64(max(1, agents)), PerAgentMeasured
	case lastPerAgent > 0:
		// An idle fleet keeps the per-agent rate it last showed.
		r.perAgent, r.perAgentBasis = lastPerAgent, PerAgentLastMeasured
	default:
		r.perAgent, r.perAgentBasis = s.taskRate(o, first), PerAgentTaskEstimate
	}
	if !recentKnown {
		// Without any usage evidence, assume each working agent burns the
		// per-agent rate.
		r.recent, r.recentBasis = float64(agents)*r.perAgent, "estimated"
	}
	switch {
	case r.need <= 0 || r.recent >= r.need:
		r.state = RateConserve
	case r.recent >= r.target || r.perAgent <= 0:
		r.state = RateHold
	default:
		r.room = int(math.Floor((r.target - r.recent) / r.perAgent))
		r.state = RateRoom
		if r.room < 1 {
			r.room, r.state = 0, RateHold
		}
	}
	return r
}

// pickStep starts at the ladder's first step and climbs while the fleet runs
// well under its target. The fast/priority top step is only for allowance
// that would still expire unused soon with the fleet at the target.
func (s *scheduler) pickStep(o *offer, r *fleetRate) (int, string) {
	cfg := s.f.cfg
	if r == nil || r.state == RateConserve || o.override {
		return 0, ""
	}
	topStandard := 0
	for i, step := range o.ladder {
		if step.Mode == ModeStandard {
			topStandard = i
		}
	}
	steps, why := 0, ""
	if r.target > 0 {
		ratio := r.recent / r.target
		switch {
		case ratio < cfg.StepUpTwoBelow:
			steps, why = 2, fmt.Sprintf("the fleet runs at %.0f%% of its target", ratio*100)
		case ratio < cfg.StepUpOneBelow:
			steps, why = 1, fmt.Sprintf("the fleet runs at %.0f%% of its target", ratio*100)
		}
	}
	step := min(steps, topStandard)
	top := len(o.ladder) - 1
	if r.timeSensitive && o.ladder[top].Mode != ModeStandard && cfg.FastMode == "auto" && !s.caution(o) {
		return top, fmt.Sprintf("about %.0f%% of one account's allowance would still expire unused within %s at the target rate", r.expiring, roughDuration(time.Duration(cfg.FastHorizonHours*float64(time.Hour))))
	}
	if step == 0 {
		why = ""
	}
	return step, why
}

// caution reports whether the account the router would use next is close to
// a short-window limit, which rules out fast and priority mode.
func (s *scheduler) caution(o *offer) bool {
	order := s.routerOrder(o)
	return len(order) > 0 && s.f.eligible(order[0], o.model).caution
}

var kindRank = map[string]int{KindRoom: 0, KindHold: 1, KindUnmetered: 2, KindConserve: 3}

// decide advises every usable capability from the fleet's shared rate. It
// reserves nothing; the room it reports is shared by all orchestrators.
func decide(f *forecaster, req Request, activity fleetactivity.Snapshot, openCode *OpenCodeUsage, lastPerAgent map[string]float64) decision {
	s := newScheduler(f, req.ContextTokens)
	d := decision{rates: map[string]*fleetRate{}, openCode: openCode, activity: activity}
	firstOffer := map[string]*offer{}
	for _, o := range s.offers {
		if o.state == OfferMetered && firstOffer[o.provider] == nil {
			firstOffer[o.provider] = o
		}
	}
	for provider := range f.pools {
		d.rates[provider] = s.fleetRate(provider, firstOffer[provider], activity.Providers[provider].Agents, lastPerAgent[provider])
	}
	if openCode != nil && !openCode.Reachable {
		d.warnings = append(d.warnings, "OpenCode Go usage tracker unreachable; its budget is unknown")
	}
	roomed := map[string]bool{}
	for _, o := range s.offers {
		var c modelChoice
		ok := false
		switch o.state {
		case OfferMetered:
			c, ok = s.meteredChoice(o, d.rates[o.provider])
		case OfferUnmetered:
			c, ok = s.unmeteredChoice(o, openCode, &d)
		}
		if ok {
			d.choices = append(d.choices, c)
			if c.room > 0 && !roomed[o.provider] {
				roomed[o.provider] = true
				d.room += c.room
			}
			continue
		}
		if o.state == OfferUnmetered {
			continue // avoided; the warning explains why
		}
		label := o.cap.Name
		if o.model != "" {
			label = o.model
		}
		why := o.reason
		if why == "" {
			why = "no capacity left above account reserves"
		}
		d.warnings = append(d.warnings, fmt.Sprintf("%s unavailable: %s", label, why))
		if !o.nextAvailable.IsZero() && o.nextAvailable.After(f.now) && (d.retryAfter.IsZero() || o.nextAvailable.Before(d.retryAfter)) {
			d.retryAfter = o.nextAvailable
		}
	}
	sort.SliceStable(d.choices, func(i, j int) bool { return kindRank[d.choices[i].kind] < kindRank[d.choices[j].kind] })
	kinds := map[string]bool{}
	stale := false
	for _, c := range d.choices {
		kinds[c.kind] = true
		stale = stale || c.stale
	}
	switch {
	case len(d.choices) == 0:
		d.posture, d.status = PostureUnavailable, StatusUnavailable
	case kinds[KindRoom]:
		d.posture = PostureRoom
	case kinds[KindHold]:
		d.posture = PostureHold
	case kinds[KindConserve]:
		d.posture = PostureConserve
	default:
		d.posture = PostureUnknown
	}
	if d.status == "" {
		d.status = StatusReady
		if stale || d.posture == PostureConserve {
			d.status = StatusDegraded
		}
	}
	if stale {
		d.warnings = append(d.warnings, "some advice relies on stale quota observations")
	}
	d.summary = summarize(d)
	return d
}

func (s *scheduler) meteredChoice(o *offer, r *fleetRate) (modelChoice, bool) {
	if r == nil {
		return modelChoice{}, false
	}
	stale := false
	for _, st := range s.routerOrder(o) {
		stale = stale || (st.metered && st.stale)
	}
	step, why := s.pickStep(o, r)
	c := modelChoice{offer: o, step: step, stale: stale}
	switch r.state {
	case RateRoom:
		c.kind, c.room, c.reason = KindRoom, r.room, "the fleet runs below its target rate"
	case RateHold:
		c.kind, c.reason = KindHold, "the fleet already uses its target rate"
	default:
		c.kind, c.reason = KindConserve, "the fleet uses allowance faster than it lasts until the resets"
	}
	switch r.perAgentBasis {
	case PerAgentTaskEstimate:
		c.reason += "; per-agent rate estimated from task size (no measured usage)"
	case PerAgentLastMeasured:
		c.reason += "; per-agent rate from the last measured fleet use"
	}
	if why != "" {
		c.reason += "; " + why
	}
	return c, true
}

func (s *scheduler) unmeteredChoice(o *offer, u *OpenCodeUsage, d *decision) (modelChoice, bool) {
	c := modelChoice{offer: o, kind: KindUnmetered, room: -1, reason: "provider reports no quota telemetry; budget unknown"}
	if o.provider != OpenCodeProvider || u == nil || !u.Reachable {
		return c, true
	}
	if u.AvoidUntil != nil {
		msg := "OpenCode Go: avoid until " + u.AvoidUntil.UTC().Format("Jan 2 15:04 MST") + " (" + u.AvoidWhy + ")"
		if !contains(d.warnings, msg) {
			d.warnings = append(d.warnings, msg)
		}
		return modelChoice{}, false
	}
	c.reason = "OpenCode Go quota from the usage tracker; room unknown"
	return c, true
}

func summarize(d decision) string {
	if len(d.choices) == 0 {
		return "No model has usable capacity from current quota evidence; start no new subagents."
	}
	models := make([]string, 0, len(d.choices))
	for _, c := range d.choices {
		models = append(models, c.offer.model)
	}
	return fmt.Sprintf("%s: prefer %s.", d.posture, strings.Join(models, ", then "))
}

// resolveOffers turns capability rules into offers. contextTokens is the
// largest single prompt a subagent needs; zero skips the context fit check.
// Each rule resolves to its first candidate model that some account can
// serve now; otherwise the first candidate explains why it is unavailable.
func (f *forecaster) resolveOffers(contextTokens int) []*offer {
	discovered := map[string]DiscoveredModel{}
	for _, m := range f.inv.Models {
		discovered[discoveredKey(m.Provider, m.ID)] = m
	}
	var offers []*offer
	for _, c := range f.cfg.Models {
		if c.Disabled {
			continue
		}
		if p, ok := f.cfg.Providers[c.Provider]; ok && p.Disabled {
			offers = append(offers, &offer{cap: c, state: OfferUnavailable, modes: []string{ModeStandard}, reason: "provider_disabled"})
			continue
		}
		candidates, excluded := f.candidates(c)
		if len(candidates) == 0 {
			o := &offer{cap: c, state: OfferUnavailable, modes: []string{ModeStandard}, reason: "model_not_available"}
			if excluded {
				o.reason = "never_advised"
			}
			offers = append(offers, o)
			continue
		}
		var chosen *offer
		for _, id := range candidates {
			o := f.buildOffer(c, id, contextTokens, discovered)
			if o.state == "" && f.anyEligible(o) {
				chosen = o
				break
			}
			if chosen == nil || (chosen.state != "" && o.state == "") {
				chosen = o
			}
		}
		offers = append(offers, chosen)
	}
	return offers
}

func discoveredKey(provider, id string) string {
	return normalizeProvider(provider) + "\x00" + strings.ToLower(id)
}

// candidates lists the model IDs a rule matches, pattern by pattern, skipping
// models on the never-advise list.
func (f *forecaster) candidates(c ModelCapability) ([]string, bool) {
	var out []string
	excluded := false
	seen := map[string]bool{}
	// One public ID may route to different models on different accounts (a
	// shared alias); if any of them is never advised, the ID is never advised.
	banned := map[string]bool{}
	for _, st := range f.accounts {
		for _, id := range st.acct.Models {
			if f.neverAdvised(st.acct, id) {
				banned[id] = true
			}
		}
	}
	for _, pattern := range c.Match {
		var ids []string
		for _, st := range f.accounts {
			if c.Provider != "" && st.acct.Provider != c.Provider {
				continue
			}
			for _, id := range st.acct.Models {
				if seen[id] || !matchModel([]string{pattern}, id) {
					continue
				}
				if banned[id] {
					excluded = true
					continue
				}
				seen[id] = true
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		out = append(out, ids...)
	}
	return out, excluded
}

// neverAdvised checks the visible ID, the ID without a routing prefix and the
// canonical model behind an alias against the never-advise list.
func (f *forecaster) neverAdvised(a *Account, id string) bool {
	names := []string{id}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		names = append(names, id[i+1:])
	}
	if canonical := a.Canonical[id]; canonical != "" {
		names = append(names, canonical)
	}
	for _, name := range names {
		if matchModel(f.cfg.NeverAdvise, name) {
			return true
		}
	}
	return false
}

func (f *forecaster) anyEligible(o *offer) bool {
	for _, st := range o.accounts {
		if f.eligible(st, o.model).ok {
			return true
		}
	}
	return false
}

// buildOffer resolves one candidate model. Its state is empty when usable,
// otherwise OfferUnavailable with a reason.
func (f *forecaster) buildOffer(c ModelCapability, id string, contextTokens int, discovered map[string]DiscoveredModel) *offer {
	o := &offer{cap: c, model: id, state: OfferUnavailable, modes: []string{ModeStandard}}
	for _, st := range f.accounts {
		if contains(st.acct.Models, id) && (c.Provider == "" || st.acct.Provider == c.Provider) {
			if o.provider == "" {
				o.provider = st.acct.Provider
			}
			if st.acct.Provider == o.provider {
				o.accounts = append(o.accounts, st)
			}
		}
	}
	info, known := discovered[discoveredKey(o.provider, id)]
	o.ladder = append([]Step(nil), c.Ladder...)
	o.capabilitySource = "configured"
	if known && len(info.Levels) > 0 {
		levels := map[string]bool{}
		for _, l := range info.Levels {
			levels[normalizeEffort(l)] = true
		}
		var allowed []Step
		for _, step := range c.Ladder {
			if levels[step.Effort] {
				allowed = append(allowed, step)
			}
		}
		o.ladder = allowed
		o.capabilitySource = "registry+configured"
	}
	if len(o.ladder) == 0 {
		o.reason = "no_supported_effort"
		return o
	}
	for _, step := range o.ladder {
		if !contains(o.efforts, step.Effort) {
			o.efforts = append(o.efforts, step.Effort)
		}
		if !contains(o.modes, step.Mode) {
			o.modes = append(o.modes, step.Mode)
		}
	}
	if contextTokens > 0 && known && info.ContextTokens > 0 && contextTokens+c.MaxOutputTokens > info.ContextTokens {
		o.reason = "context_too_small"
		return o
	}
	_, o.override = f.inv.RoutingOverrides[o.provider]
	o.state = ""
	return o
}

func effortMult(effort string) float64 {
	if m, ok := effortCostMultiplier[effort]; ok {
		return m
	}
	return 1
}

func (o *offer) modeMult(mode string) float64 {
	if mode == ModeStandard || mode == "" {
		return 1
	}
	if m, ok := o.cap.ModeCostMultiplier[mode]; ok && m > 0 {
		return m
	}
	return 2
}

// slotCost estimates one subagent task's allowance cost in the account's units.
func (f *forecaster) slotCost(o *offer, st *accountState, effort, mode string, inputTokens int) float64 {
	if st.capacityKnown && o.cap.Price != nil {
		output := float64(f.cfg.TaskOutputTokens) * effortMult(effort)
		base := (float64(inputTokens)*o.cap.Price.Input + output*o.cap.Price.Output) / 1e6
		return base * o.modeMult(mode)
	}
	return f.cfg.UnmeteredSubagentPercent / 100 * st.capacity * effortMult(effort) * o.modeMult(mode)
}
