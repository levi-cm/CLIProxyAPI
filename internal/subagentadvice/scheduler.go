package subagentadvice

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Offer states.
const (
	OfferSurplus     = "surplus"
	OfferNormal      = "normal"
	OfferTight       = "tight"
	OfferUnmetered   = "unmetered"
	OfferUnavailable = "unavailable"
)

var effortCostMultiplier = map[string]float64{"none": 0.2, "minimal": 0.3, "low": 0.5, "medium": 0.75, "high": 1, "xhigh": 1.5, "max": 2.5}

type offer struct {
	cap              ModelCapability
	model            string
	provider         string
	efforts          []string
	modes            []string
	capabilitySource string
	accounts         []*accountState
	state            string
	reason           string
	nextAvailable    time.Time
	override         bool
}

// Spending postures tell the orchestrator how freely to use subagents. They
// never cap the subagent count; the orchestrator decides that from its work.
const (
	PostureSpendFreely = "spend_freely"
	PostureNormal      = "normal"
	PostureConserve    = "conserve"
	PostureUnavailable = "unavailable"
)

// modelChoice is the advice for one capability.
type modelChoice struct {
	offer  *offer
	kind   string
	effort string
	mode   string
	cost   float64   // one task, in the reference account's units
	budget float64   // tasks the evidence supports; negative means unknown
	until  time.Time // when the budget ends (next relevant reset)
	stale  bool
	reason string
}

type decision struct {
	status     string
	posture    string
	choices    []modelChoice
	warnings   []string
	retryAfter time.Time
	summary    string
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

func (o *offer) defaultEffort() string {
	if contains(o.efforts, o.cap.DefaultEffort) {
		return o.cap.DefaultEffort
	}
	return o.efforts[0]
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
	metered := false
	for _, st := range eligible {
		metered = metered || st.metered
	}
	if !metered {
		o.state, o.reason = OfferUnmetered, "no_quota_telemetry"
		return
	}
	effort := o.defaultEffort()
	if !o.override {
		for _, st := range s.surplusAccounts(o) {
			if st.surplusUnits >= s.f.slotCost(o, st, effort, ModeStandard, s.input) {
				o.state, o.reason = OfferSurplus, ""
				return
			}
		}
	}
	for _, st := range eligible {
		if !st.metered {
			o.state, o.reason = OfferUnmetered, "metered_accounts_tight"
			return
		}
		if s.f.fits(st.acct.Provider, map[string]float64{st.acct.ID: s.f.slotCost(o, st, effort, ModeStandard, s.input)}) {
			o.state, o.reason = OfferNormal, ""
			return
		}
	}
	o.state, o.reason = OfferTight, "projected_shortfall_before_reset"
}

// surplusAccounts lists eligible metered accounts with usable surplus whose
// short windows are not near their limit, largest surplus first.
func (s *scheduler) surplusAccounts(o *offer) []*accountState {
	var out []*accountState
	for _, st := range s.routerOrder(o) {
		if st.metered && st.surplusUnits > 0 && !s.f.eligible(st, o.model).caution {
			out = append(out, st)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].surplusUnits > out[j].surplusUnits })
	return out
}

// surplusChoice spends allowance projected to expire: the highest allowed
// effort and, when configured and affordable, fast or priority mode.
func (s *scheduler) surplusChoice(o *offer) (modelChoice, bool) {
	accounts := s.surplusAccounts(o)
	if o.override || len(accounts) == 0 {
		return modelChoice{}, false
	}
	best := accounts[0]
	total, capacity := 0.0, 0.0
	until, stale := time.Time{}, false
	for _, st := range accounts {
		total += st.surplusUnits
		capacity += st.capacity
		stale = stale || st.stale
		if until.IsZero() || st.long.ResetAt.Before(until) {
			until = st.long.ResetAt
		}
	}
	for i := len(o.efforts) - 1; i >= 0; i-- {
		effort := o.efforts[i]
		var modes []string
		if s.f.cfg.FastMode == "auto" {
			for _, m := range o.modes {
				if m != ModeStandard {
					modes = append(modes, m)
				}
			}
		}
		modes = append(modes, ModeStandard)
		for _, mode := range modes {
			cost := s.f.slotCost(o, best, effort, mode, s.input)
			if cost > best.surplusUnits || !s.f.fits(best.acct.Provider, map[string]float64{best.acct.ID: cost}) {
				continue
			}
			// Spend only surplus that also sits above each account's reserve.
			spend := map[*accountState]float64{}
			for _, st := range accounts {
				spend[st] = math.Min(st.surplusUnits, s.room(st))
			}
			budget := s.tasks(o, effort, mode, spend)
			if budget < 1 {
				continue
			}
			reason := fmt.Sprintf("about %.0f%% of the allowance on %d account(s) is projected unused at reset; spend it", total/capacity*100, len(accounts))
			return modelChoice{offer: o, kind: KindSurplus, effort: effort, mode: mode, cost: cost, budget: budget, until: until, stale: stale, reason: reason}, true
		}
	}
	return modelChoice{}, false
}

// room is the spend an account can take without breaching its reserve.
func (s *scheduler) room(st *accountState) float64 {
	pool := s.f.pools[st.acct.Provider]
	return math.Max(0, st.remainingUnits-st.reserveUnits(s.f.cfg, pool != nil && pool.demandKnown))
}

// tasks counts the subagent tasks a per-account spend pays for, pricing each
// account's share with that account's own task cost.
func (s *scheduler) tasks(o *offer, effort, mode string, spend map[*accountState]float64) float64 {
	n := 0.0
	for st, units := range spend {
		cost := s.f.slotCost(o, st, effort, mode, s.input)
		if units > 0 && cost > 0 && !math.IsInf(cost, 0) && !math.IsNaN(cost) {
			n += math.Floor(units / cost)
		}
	}
	return n
}

// headroom spreads the largest extra spend the provider can absorb on these
// accounts (router order) without breaching reserves or creating a new
// shortfall before its resets, and returns each account's share.
func (s *scheduler) headroom(accounts []*accountState) map[*accountState]float64 {
	if len(accounts) == 0 {
		return nil
	}
	provider := accounts[0].acct.Provider
	hard := 0.0
	for _, st := range accounts {
		hard += s.room(st)
	}
	spread := func(x float64) map[string]float64 {
		out := map[string]float64{}
		for _, st := range accounts {
			take := math.Min(s.room(st), x)
			if take > 0 {
				out[st.acct.ID] = take
				x -= take
			}
		}
		return out
	}
	lo, hi := 0.0, hard
	if s.f.fits(provider, spread(hi)) {
		lo = hi
	} else {
		for i := 0; i < 40 && hi-lo > hard*1e-4; i++ {
			mid := (lo + hi) / 2
			if s.f.fits(provider, spread(mid)) {
				lo = mid
			} else {
				hi = mid
			}
		}
	}
	byID := spread(lo)
	out := make(map[*accountState]float64, len(byID))
	for _, st := range accounts {
		if units := byID[st.acct.ID]; units > 0 {
			out[st] = units
		}
	}
	return out
}

// defaultChoice advises the default effort in standard mode with the budget
// the forecast leaves before the provider's resets. Conserve advice uses the
// lowest effort and the hard capacity left above the account floors.
func (s *scheduler) defaultChoice(o *offer, conserve bool) (modelChoice, bool) {
	var metered []*accountState
	stale := false
	for _, st := range s.routerOrder(o) {
		if st.metered {
			metered = append(metered, st)
			stale = stale || st.stale
		}
	}
	effort := o.defaultEffort()
	kind, reason := KindDefault, "forecast capacity covers demand until the next reset"
	switch {
	case o.state == OfferUnmetered:
		return modelChoice{offer: o, kind: KindUnmetered, effort: effort, mode: ModeStandard, budget: -1, reason: "provider reports no quota telemetry; budget unknown"}, true
	case conserve:
		effort, kind, reason = o.efforts[0], KindConserve, "pool is projected short before its next reset; lowest effort, few subagents"
	}
	cost := s.f.slotCost(o, metered[0], effort, ModeStandard, s.input)
	var spend map[*accountState]float64
	if conserve {
		// The pool is short anyway; only capacity above the reserves matters.
		spend = map[*accountState]float64{}
		for _, st := range metered {
			spend[st] = s.room(st)
		}
	} else {
		spend = s.headroom(metered)
	}
	budget := s.tasks(o, effort, ModeStandard, spend)
	if budget < 1 {
		// Accounts past their allowance may still serve on credits; their
		// budget is the credit balance, which is not estimated here.
		for _, st := range metered {
			if st.acct.ServingOnCredits {
				return modelChoice{offer: o, kind: KindConserve, effort: o.efforts[0], mode: ModeStandard, cost: cost, budget: -1, stale: stale,
					reason: "allowance exhausted; accounts serve on paid credits, budget unknown"}, true
			}
		}
		return modelChoice{}, false
	}
	if metered[0].paceBasis == PaceUnknown && !conserve {
		reason = "usage pace unknown, so only account reserves are checked"
	}
	var until time.Time
	if pool := s.f.pools[o.provider]; pool != nil {
		until = pool.nextReset
	}
	return modelChoice{offer: o, kind: kind, effort: effort, mode: ModeStandard, cost: cost, budget: budget, until: until, stale: stale, reason: reason}, true
}

var kindRank = map[string]int{KindSurplus: 0, KindDefault: 1, KindUnmetered: 2, KindConserve: 3}

// decide advises every usable capability, in preference order: expiring
// allowance first, then normal capacity, unmetered providers, and conserve
// options last. It never limits how many subagents run.
func decide(f *forecaster, req Request) decision {
	s := newScheduler(f, req.ContextTokens)
	d := decision{}
	for _, o := range s.offers {
		var c modelChoice
		ok := false
		switch o.state {
		case OfferSurplus:
			if c, ok = s.surplusChoice(o); !ok {
				c, ok = s.defaultChoice(o, false)
			}
		case OfferNormal, OfferUnmetered:
			c, ok = s.defaultChoice(o, false)
		case OfferTight:
			c, ok = s.defaultChoice(o, true)
		}
		if ok {
			d.choices = append(d.choices, c)
			continue
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
		if o.state == OfferTight || o.state == OfferNormal || o.state == OfferSurplus {
			if pool := f.pools[o.provider]; pool != nil && pool.nextReset.After(f.now) && (d.retryAfter.IsZero() || pool.nextReset.Before(d.retryAfter)) {
				d.retryAfter = pool.nextReset
			}
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
	case kinds[KindSurplus]:
		d.posture = PostureSpendFreely
	case kinds[KindDefault] || kinds[KindUnmetered]:
		d.posture = PostureNormal
	default:
		d.posture = PostureConserve
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
	o.efforts = append([]string(nil), c.Efforts...)
	o.capabilitySource = "configured"
	if known && len(info.Levels) > 0 {
		levels := map[string]bool{}
		for _, l := range info.Levels {
			levels[normalizeEffort(l)] = true
		}
		var allowed []string
		for _, e := range c.Efforts {
			if levels[e] {
				allowed = append(allowed, e)
			}
		}
		o.efforts = allowed
		o.capabilitySource = "registry+configured"
	}
	if len(o.efforts) == 0 {
		o.reason = "no_supported_effort"
		return o
	}
	o.modes = append([]string(nil), c.Modes...)
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
