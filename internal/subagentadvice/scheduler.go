package subagentadvice

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Offer states.
const (
	OfferMetered     = "metered"
	OfferUnmetered   = "unmetered"
	OfferUnavailable = "unavailable"
)

// Step levels of a capability, from cheapest to most expensive.
const (
	levelConserve = "conserve"
	levelNormal   = "normal"
	levelUseMore  = "use_more"
	levelFast     = "fast"
)

type offer struct {
	cap      ModelCapability
	model    string
	provider string
	// steps maps each level to the step advised, limited to supported efforts.
	steps            map[string]Step
	efforts          []string
	modes            []string
	capabilitySource string
	accounts         []*accountState
	state            string
	reason           string
	nextAvailable    time.Time
	override         bool
}

// modelChoice is the advice for one capability.
type modelChoice struct {
	offer   *offer
	posture string
	step    Step
	stale   bool
	reason  string
}

type decision struct {
	status     string
	postures   []ProviderPosture
	choices    []modelChoice
	warnings   []string
	retryAfter time.Time
	summary    string
	openCode   *OpenCodeUsage
}

type scheduler struct {
	f      *forecaster
	offers []*offer
}

func newScheduler(f *forecaster, contextTokens int) *scheduler {
	s := &scheduler{f: f}
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

// caution reports whether the account the router would use next is close to
// a short-window limit, which rules out fast and priority mode.
func (s *scheduler) caution(o *offer) bool {
	order := s.routerOrder(o)
	return len(order) > 0 && s.f.eligible(order[0], o.model).caution
}

var postureRank = map[string]int{PostureNormal: 0, PostureUseMore: 0, PostureConserve: 1, PostureAvoid: 2}

// decide gives each provider a posture from its usage trend and each usable
// capability the step of that posture. It reserves nothing and never gives a
// number of agents.
func decide(f *forecaster, req Request, openCode *OpenCodeUsage) decision {
	s := newScheduler(f, req.ContextTokens)
	d := decision{openCode: openCode}
	trends := map[string]*poolTrend{}
	decided := map[string]int{}
	for _, o := range s.offers {
		c, posture := s.choose(o, openCode, trends, &d)
		// A provider's first capability sets its posture, unless it is
		// unavailable and a later one is not.
		if i, ok := decided[posture.Provider]; !ok && posture.Provider != "" {
			decided[posture.Provider] = len(d.postures)
			d.postures = append(d.postures, posture)
		} else if ok && d.postures[i].Posture == PostureAvoid && c != nil && c.posture != PostureAvoid {
			d.postures[i] = posture
		}
		if c != nil {
			d.choices = append(d.choices, *c)
		}
	}
	sort.SliceStable(d.postures, func(i, j int) bool {
		return postureRank[d.postures[i].Posture] < postureRank[d.postures[j].Posture]
	})
	sort.SliceStable(d.choices, func(i, j int) bool {
		return postureRank[d.choices[i].posture] < postureRank[d.choices[j].posture]
	})
	stale, usable := false, 0
	for _, c := range d.choices {
		stale = stale || c.stale
		if c.posture != PostureAvoid {
			usable++
		}
	}
	switch {
	case usable == 0:
		d.status = StatusUnavailable
	case stale:
		d.status = StatusDegraded
		d.warnings = append(d.warnings, "some advice relies on stale quota observations")
	default:
		d.status = StatusReady
	}
	d.summary = summarize(d)
	return d
}

// choose advises one capability and returns its provider's posture.
func (s *scheduler) choose(o *offer, u *OpenCodeUsage, trends map[string]*poolTrend, d *decision) (*modelChoice, ProviderPosture) {
	f := s.f
	provider := o.provider
	if provider == "" {
		provider = o.cap.Provider
	}
	if o.state == OfferUnavailable {
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
		return nil, ProviderPosture{Provider: provider, Posture: PostureAvoid, Reason: "no usable account (" + why + ")"}
	}
	if provider == OpenCodeProvider {
		switch {
		case u == nil:
			return nil, ProviderPosture{Provider: provider, Posture: PostureAvoid, Reason: "budget unknown (no usage tracker configured)"}
		case !u.Reachable:
			return nil, ProviderPosture{Provider: provider, Posture: PostureAvoid, Reason: "budget unknown (usage tracker unreachable)"}
		case u.AvoidUntil != nil:
			// Quota known but nearly gone: keep the model as a last resort.
			reason := u.AvoidWhy + "; only when Claude and Codex can't serve"
			return &modelChoice{offer: o, posture: PostureAvoid, step: o.steps[levelNormal], reason: reason}, ProviderPosture{Provider: provider, Posture: PostureAvoid, Reason: reason}
		}
		reason := "OpenCode Go quota from the usage tracker"
		return &modelChoice{offer: o, posture: PostureNormal, step: o.steps[levelNormal], reason: reason}, ProviderPosture{Provider: provider, Posture: PostureNormal, Reason: reason}
	}
	t := trends[provider]
	if t == nil {
		t = f.poolTrend(provider)
		trends[provider] = t
	}
	level, posture, reason := s.level(o, t)
	stale := false
	for _, st := range s.routerOrder(o) {
		stale = stale || (st.metered && st.stale)
	}
	pp := ProviderPosture{Provider: provider, Posture: posture, Reason: reason, Trend: t.view()}
	return &modelChoice{offer: o, posture: posture, step: o.steps[level], stale: stale, reason: reason}, pp
}

// level picks the step from the trend: normal by default and while the trend
// is unknown, use-more only well under budget (plus fast/priority when
// allowance expires unused soon at the trend), conserve above budget.
func (s *scheduler) level(o *offer, t *poolTrend) (string, string, string) {
	cfg := s.f.cfg
	if !t.known {
		return levelNormal, PostureNormal, "trend unknown (" + t.unknown + "); normal effort"
	}
	ratio := t.ratio()
	switch {
	case o.override:
		return levelNormal, PostureNormal, fmt.Sprintf("trend %.2f of budget; an operator routing override is active", ratio)
	case ratio > cfg.ConserveAboveRatio:
		return levelConserve, PostureConserve, fmt.Sprintf("trend %.2f of budget: allowance runs out before it resets at this pace; run fewer subagents", ratio)
	case ratio >= cfg.XHighBelowRatio:
		return levelNormal, PostureNormal, fmt.Sprintf("trend %.2f of budget", ratio)
	}
	reason := fmt.Sprintf("trend %.2f of budget: allowance would expire unused at this pace", ratio)
	fast := o.steps[levelFast]
	if t.expiring >= cfg.FastMinExpiringPercent && cfg.FastMode == "auto" && fast != o.steps[levelUseMore] && !s.caution(o) {
		reason += fmt.Sprintf("; %.0f%% of one account expires unused within %s at the trend", t.expiring, roughDuration(time.Duration(cfg.FastHorizonHours*float64(time.Hour))))
		return levelFast, PostureUseMore, reason
	}
	return levelUseMore, PostureUseMore, reason
}

// postureName is the human name of a posture.
func postureName(p string) string {
	if p == PostureUseMore {
		return "use more"
	}
	return p
}

// postureLine is the first line: every provider's posture.
func postureLine(postures []ProviderPosture) string {
	if len(postures) == 0 {
		return "unavailable: start no new subagents"
	}
	parts := make([]string, 0, len(postures))
	for _, p := range postures {
		parts = append(parts, p.Provider+" "+postureName(p.Posture))
	}
	return strings.Join(parts, ", ")
}

func summarize(d decision) string {
	if d.status == StatusUnavailable {
		return "No model has usable capacity from current quota evidence; start no new subagents."
	}
	return postureLine(d.postures)
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
	o.capabilitySource = "configured"
	supported := func(Step) bool { return true }
	if known && len(info.Levels) > 0 {
		levels := map[string]bool{}
		for _, l := range info.Levels {
			levels[normalizeEffort(l)] = true
		}
		supported = func(step Step) bool { return levels[step.Effort] }
		o.capabilitySource = "registry+configured"
	}
	if !supported(c.Normal) {
		o.reason = "no_supported_effort"
		return o
	}
	o.steps = map[string]Step{levelNormal: c.Normal}
	for level, step := range map[string]Step{levelConserve: c.Conserve, levelUseMore: c.UseMore, levelFast: c.Fast} {
		if !supported(step) {
			// An unsupported step falls back to the normal one.
			step = c.Normal
		}
		o.steps[level] = step
	}
	var efforts []string
	for _, level := range []string{levelConserve, levelNormal, levelUseMore, levelFast} {
		step := o.steps[level]
		efforts = append(efforts, step.Effort)
		if !contains(o.modes, step.Mode) {
			o.modes = append(o.modes, step.Mode)
		}
	}
	o.efforts = sortEfforts(efforts)
	if contextTokens > 0 && known && info.ContextTokens > 0 && contextTokens+c.MaxOutputTokens > info.ContextTokens {
		o.reason = "context_too_small"
		return o
	}
	_, o.override = f.inv.RoutingOverrides[o.provider]
	o.state = ""
	return o
}
