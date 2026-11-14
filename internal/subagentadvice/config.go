// Package subagentadvice forecasts provider quota and turns it into
// deterministic, read-only subagent advice for orchestrators: a posture per
// provider and, per model, the reasoning effort and mode. Postures follow each
// pool's usage trend over the last days against an even daily budget. It
// reserves nothing, runs nothing and never gives a number of agents.
package subagentadvice

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
)

// Canonical reasoning effort levels, ordered from cheapest to most expensive.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Execution modes. Fast is Anthropic's speed tier; priority is the OpenAI
// service tier. Both spend allowance faster and are selected only for
// allowance that would otherwise expire unused soon.
const (
	ModeStandard = "standard"
	ModeFast     = "fast"
	ModePriority = "priority"
)

// Config is the `subagent-advisor` configuration section. Zero values select
// the defaults returned by DefaultConfig.
type Config struct {
	// Enabled defaults to true; false makes the endpoints return 503.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MinRefreshSeconds and MaxRefreshSeconds bound how long orchestrators may
	// keep advice before asking again (defaults 120 and 3600). Within them the
	// interval follows the situation: long while quota is comfortable, shorter
	// while spending surplus or when capacity is tight, and never past the next
	// known reset or recovery.
	MinRefreshSeconds int `yaml:"min-refresh-seconds,omitempty" json:"min-refresh-seconds,omitempty"`
	MaxRefreshSeconds int `yaml:"max-refresh-seconds,omitempty" json:"max-refresh-seconds,omitempty"`
	// NeverAdvise lists model ID globs that are never recommended, even when a
	// capability rule matches them (default ["claude-fable-*"]).
	NeverAdvise []string `yaml:"never-advise,omitempty" json:"never-advise,omitempty"`
	// StaleAfterSeconds marks a quota observation stale (default 900).
	StaleAfterSeconds int `yaml:"stale-after-seconds,omitempty" json:"stale-after-seconds,omitempty"`
	// ExpiredAfterSeconds makes a quota observation unusable (default 21600).
	ExpiredAfterSeconds int `yaml:"expired-after-seconds,omitempty" json:"expired-after-seconds,omitempty"`
	// SurplusHorizonHours counts allowance projected to expire unused at resets
	// within this horizon (default 192: every weekly window and its refill).
	// The goal is to use the weekly and monthly allowances fully.
	SurplusHorizonHours float64 `yaml:"surplus-horizon-hours,omitempty" json:"surplus-horizon-hours,omitempty"`
	// SafetyFactor multiplies forecast demand when checking continuity (default 1.25).
	SafetyFactor float64 `yaml:"safety-factor,omitempty" json:"safety-factor,omitempty"`
	// ReservePercent is the per-account floor used when demand is unknown (default 5).
	ReservePercent float64 `yaml:"reserve-percent,omitempty" json:"reserve-percent,omitempty"`
	// ShortWindowCautionPercent stops fast and priority mode on an account whose
	// short window (for example 5h) is at least this full (default 85). Short
	// windows never limit budgets; they only block an account while closed.
	ShortWindowCautionPercent float64 `yaml:"short-window-caution-percent,omitempty" json:"short-window-caution-percent,omitempty"`
	// ShortWindowBlockPercent treats a short window this full as closed (default 97).
	ShortWindowBlockPercent float64 `yaml:"short-window-block-percent,omitempty" json:"short-window-block-percent,omitempty"`
	// InFlightRequestPercent is the assumed remaining cost of each in-flight request (default 0.05).
	InFlightRequestPercent float64 `yaml:"in-flight-request-percent,omitempty" json:"in-flight-request-percent,omitempty"`
	// FastMode is "auto" (default) or "off".
	FastMode string `yaml:"fast-mode,omitempty" json:"fast-mode,omitempty"`
	// ReuseSeconds is how long orchestrators reuse advice before asking again
	// (default 300), within the refresh bounds and never past a known change.
	ReuseSeconds int `yaml:"reuse-seconds,omitempty" json:"reuse-seconds,omitempty"`
	// TrendDays is how many past days the usage trend averages (default 3).
	// The trend needs at least a day of history; otherwise it is unknown and
	// advice stays at the normal step.
	TrendDays float64 `yaml:"trend-days,omitempty" json:"trend-days,omitempty"`
	// CreditSpendDays spreads credits above the protected floors over this many
	// days in the daily budget (default 7), while routing spends credits.
	// Credits never unlock fast or priority mode.
	CreditSpendDays float64 `yaml:"credit-spend-days,omitempty" json:"credit-spend-days,omitempty"`
	// XHighBelowRatio advises the use-more step (extra high) only while the
	// trend is below this share of the even daily budget (default 0.5).
	XHighBelowRatio float64 `yaml:"xhigh-below-ratio,omitempty" json:"xhigh-below-ratio,omitempty"`
	// ConserveAboveRatio advises the conserve step while the trend is above
	// this share of the even daily budget (default 1.0).
	ConserveAboveRatio float64 `yaml:"conserve-above-ratio,omitempty" json:"conserve-above-ratio,omitempty"`
	// FastHorizonHours and FastMinExpiringPercent gate the fast/priority
	// step: on top of the use-more condition, at least this much allowance
	// (percent of one account) must expire unused within the horizon at the
	// trend (defaults 24 and 50).
	FastHorizonHours       float64 `yaml:"fast-horizon-hours,omitempty" json:"fast-horizon-hours,omitempty"`
	FastMinExpiringPercent float64 `yaml:"fast-min-expiring-percent,omitempty" json:"fast-min-expiring-percent,omitempty"`
	// OpenCodeUsageURL is the usage tracker's JSON endpoint for OpenCode Go
	// windows. Empty leaves OpenCode Go's budget unknown.
	OpenCodeUsageURL string `yaml:"opencode-go-usage-url,omitempty" json:"opencode-go-usage-url,omitempty"`
	// OpenCodeAvoidPercent avoids OpenCode Go while a fresh window has at most
	// this much left (default 5).
	OpenCodeAvoidPercent float64 `yaml:"opencode-go-avoid-percent,omitempty" json:"opencode-go-avoid-percent,omitempty"`
	// Providers holds per-provider capacity estimates.
	Providers map[string]ProviderConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
	// AccountCapacityUnits overrides the capacity estimate for one credential ID.
	AccountCapacityUnits map[string]float64 `yaml:"account-capacity-units,omitempty" json:"account-capacity-units,omitempty"`
	// Models lists subagent capability definitions in preference order. When
	// empty, the built-in defaults apply.
	Models []ModelCapability `yaml:"models,omitempty" json:"models,omitempty"`
	// Prices estimates historical request cost (USD-equivalent per million tokens) by model glob.
	Prices map[string]Price `yaml:"prices,omitempty" json:"prices,omitempty"`
}

// ProviderConfig describes one provider pool.
type ProviderConfig struct {
	// CapacityUnits estimates one account's full long-window allowance in the
	// provider's cost units (for Claude: Opus 5.5-equivalent USD at the
	// configured prices). It turns the request ledger into percent of an
	// account for the usage trend. Zero means unknown.
	CapacityUnits float64 `yaml:"capacity-units,omitempty" json:"capacity-units,omitempty"`
	// PlanCapacityUnits overrides CapacityUnits by subscription plan (for example "pro").
	PlanCapacityUnits map[string]float64 `yaml:"plan-capacity-units,omitempty" json:"plan-capacity-units,omitempty"`
	// DefaultPrice estimates historical cost for models without a price entry.
	DefaultPrice *Price `yaml:"default-price,omitempty" json:"default-price,omitempty"`
	// CreditsPerAllowance is how many provider credits one account's full
	// long-window allowance is worth at our workload (Codex Pro: ~12800 per
	// week). It turns credit balances above the protected floor into capacity
	// that advice spends. Zero ignores credits.
	CreditsPerAllowance float64 `yaml:"credits-per-allowance,omitempty" json:"credits-per-allowance,omitempty"`
	// Disabled removes the provider from planning.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Price is a USD-equivalent price per million tokens.
type Price struct {
	Input     float64 `yaml:"input" json:"input"`
	Output    float64 `yaml:"output" json:"output"`
	CacheRead float64 `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
}

// ModelCapability is one subagent model rule. Only listed efforts and modes
// are ever selected, and only when the discovered model supports them.
type ModelCapability struct {
	// Name is a stable label such as "claude-opus-5.5".
	Name string `yaml:"name" json:"name"`
	// Match lists model ID globs; the first currently available match is used.
	Match []string `yaml:"match" json:"match"`
	// Provider restricts the match to accounts of this provider. Empty matches any.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Normal is the step advised by default and while the trend is unknown
	// (default effort high).
	Normal Step `yaml:"normal,omitempty" json:"normal,omitempty"`
	// UseMore is advised while the trend is clearly under budget (default:
	// the normal step).
	UseMore Step `yaml:"use-more,omitempty" json:"use-more,omitempty"`
	// Fast is advised on top of use-more when allowance expires unused soon;
	// empty means use-more.
	Fast Step `yaml:"fast,omitempty" json:"fast,omitempty"`
	// Conserve is advised while the trend is above budget (default: the normal step).
	Conserve Step `yaml:"conserve,omitempty" json:"conserve,omitempty"`
	// Price estimates the model's cost in its provider's units.
	Price *Price `yaml:"price,omitempty" json:"price,omitempty"`
	// MaxOutputTokens is the output budget used for the context fit check (default 32000).
	MaxOutputTokens int `yaml:"max-output-tokens,omitempty" json:"max-output-tokens,omitempty"`
	// Harnesses maps an orchestrator harness (for example "claude-code",
	// "codex", "opencode") to the model name that harness uses to start this
	// capability. An empty name means the resolved proxy model ID.
	Harnesses map[string]string `yaml:"harnesses,omitempty" json:"harnesses,omitempty"`
	// Disabled removes the rule without deleting it.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Step is one reasoning effort and execution mode.
type Step struct {
	Effort string `yaml:"effort" json:"effort"`
	// Mode is standard (default), fast or priority.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

func boolPtr(v bool) *bool { return &v }

// DefaultConfig returns the built-in scheduler configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:                   boolPtr(true),
		MinRefreshSeconds:         120,
		MaxRefreshSeconds:         3600,
		NeverAdvise:               []string{"claude-fable-*"},
		StaleAfterSeconds:         900,
		ExpiredAfterSeconds:       21600,
		SurplusHorizonHours:       192,
		SafetyFactor:              1.25,
		ReservePercent:            5,
		ShortWindowCautionPercent: 85,
		ShortWindowBlockPercent:   97,
		InFlightRequestPercent:    0.05,
		FastMode:                  "auto",
		ReuseSeconds:              300,
		TrendDays:                 3,
		CreditSpendDays:           7,
		XHighBelowRatio:           0.5,
		ConserveAboveRatio:        1.0,
		FastHorizonHours:          24,
		FastMinExpiringPercent:    50,
		OpenCodeAvoidPercent:      5,
		Providers: map[string]ProviderConfig{
			"claude": {
				// Calibrated 2026-11-15 against the request ledger: one account's
				// week ran out after about 1900 priced units, another showed 6%
				// used after 128. A rough estimate; tune it in config.
				CapacityUnits: 2000,
				DefaultPrice:  &Price{Input: 4, Output: 20, CacheRead: 0.2},
			},
			"codex":       {CreditsPerAllowance: 12800},
			"opencode-go": {},
		},
		Models: []ModelCapability{
			{
				Name:            "claude-opus-5.5",
				Match:           []string{"claude-opus-5-5"},
				Provider:        "claude",
				Normal:          Step{Effort: "high"},
				UseMore:         Step{Effort: "xhigh"},
				Fast:            Step{Effort: "xhigh", Mode: ModeFast},
				Conserve:        Step{Effort: "high"},
				Price:           &Price{Input: 4, Output: 20, CacheRead: 0.2},
				MaxOutputTokens: 64000,
				Harnesses:       map[string]string{"claude-code": "opus", "opencode": ""},
			},
			{
				Name:            "gpt-6.1-sol",
				Match:           []string{"gpt-6.1-sol"},
				Provider:        "codex",
				Normal:          Step{Effort: "high"},
				UseMore:         Step{Effort: "xhigh"},
				Fast:            Step{Effort: "xhigh", Mode: ModePriority},
				Conserve:        Step{Effort: "medium"},
				Price:           &Price{Input: 2.5, Output: 15, CacheRead: 0.25},
				MaxOutputTokens: 32000,
				Harnesses:       map[string]string{"codex": "", "opencode": ""},
			},
			{
				Name:            "deepseek-4.1",
				Match:           []string{"opencode-go/deepseek-v4.1*", "deepseek-v4.1*"},
				Provider:        "opencode-go",
				Normal:          Step{Effort: "high"},
				MaxOutputTokens: 32000,
				Harnesses:       map[string]string{"opencode": ""},
			},
		},
		Prices: map[string]Price{
			"claude-opus-*":   {Input: 4, Output: 20, CacheRead: 0.2},
			"claude-fable-*":  {Input: 8, Output: 40, CacheRead: 0.2},
			"claude-mythos-*": {Input: 8, Output: 40, CacheRead: 0.2},
			"claude-sonnet-*": {Input: 2, Output: 10, CacheRead: 0.1},
			"claude-haiku-*":  {Input: 0.1, Output: 0.5, CacheRead: 0.01},
		},
	}
}

// Resolve merges cfg over the defaults. Maps and model lists replace the
// defaults key by key or as a whole, so an operator can override one provider
// without restating the others.
func (cfg Config) Resolve() Config {
	out := DefaultConfig()
	if cfg.Enabled != nil {
		out.Enabled = boolPtr(*cfg.Enabled)
	}
	setInt := func(dst *int, v int) {
		if v != 0 {
			*dst = v
		}
	}
	setFloat := func(dst *float64, v float64) {
		if v != 0 {
			*dst = v
		}
	}
	setInt(&out.MinRefreshSeconds, cfg.MinRefreshSeconds)
	setInt(&out.MaxRefreshSeconds, cfg.MaxRefreshSeconds)
	if cfg.NeverAdvise != nil {
		// An explicit list replaces the default; an empty list allows every model.
		out.NeverAdvise = append([]string{}, cfg.NeverAdvise...)
	}
	setInt(&out.StaleAfterSeconds, cfg.StaleAfterSeconds)
	setInt(&out.ExpiredAfterSeconds, cfg.ExpiredAfterSeconds)
	setFloat(&out.SurplusHorizonHours, cfg.SurplusHorizonHours)
	setFloat(&out.SafetyFactor, cfg.SafetyFactor)
	setFloat(&out.ReservePercent, cfg.ReservePercent)
	setFloat(&out.ShortWindowCautionPercent, cfg.ShortWindowCautionPercent)
	setFloat(&out.ShortWindowBlockPercent, cfg.ShortWindowBlockPercent)
	setFloat(&out.InFlightRequestPercent, cfg.InFlightRequestPercent)
	if v := strings.TrimSpace(cfg.FastMode); v != "" {
		out.FastMode = strings.ToLower(v)
	}
	setInt(&out.ReuseSeconds, cfg.ReuseSeconds)
	setFloat(&out.TrendDays, cfg.TrendDays)
	setFloat(&out.CreditSpendDays, cfg.CreditSpendDays)
	setFloat(&out.XHighBelowRatio, cfg.XHighBelowRatio)
	setFloat(&out.ConserveAboveRatio, cfg.ConserveAboveRatio)
	setFloat(&out.FastHorizonHours, cfg.FastHorizonHours)
	setFloat(&out.FastMinExpiringPercent, cfg.FastMinExpiringPercent)
	setFloat(&out.OpenCodeAvoidPercent, cfg.OpenCodeAvoidPercent)
	if v := strings.TrimSpace(cfg.OpenCodeUsageURL); v != "" {
		out.OpenCodeUsageURL = v
	}
	for name, p := range cfg.Providers {
		key := normalizeProvider(name)
		base := out.Providers[key]
		if p.CapacityUnits != 0 {
			base.CapacityUnits = p.CapacityUnits
		}
		if len(p.PlanCapacityUnits) > 0 {
			base.PlanCapacityUnits = make(map[string]float64, len(p.PlanCapacityUnits))
			for plan, units := range p.PlanCapacityUnits {
				base.PlanCapacityUnits[strings.ToLower(strings.TrimSpace(plan))] = units
			}
		}
		if p.DefaultPrice != nil {
			price := *p.DefaultPrice
			base.DefaultPrice = &price
		}
		if p.CreditsPerAllowance != 0 {
			base.CreditsPerAllowance = p.CreditsPerAllowance
		}
		base.Disabled = p.Disabled
		out.Providers[key] = base
	}
	if len(cfg.AccountCapacityUnits) > 0 {
		out.AccountCapacityUnits = make(map[string]float64, len(cfg.AccountCapacityUnits))
		for id, units := range cfg.AccountCapacityUnits {
			out.AccountCapacityUnits[strings.TrimSpace(id)] = units
		}
	}
	if len(cfg.Models) > 0 {
		out.Models = append([]ModelCapability(nil), cfg.Models...)
	}
	for glob, price := range cfg.Prices {
		out.Prices[glob] = price
	}
	for i := range out.Models {
		m := &out.Models[i]
		m.Provider = normalizeProvider(m.Provider)
		m.Match = append([]string(nil), m.Match...)
		m.Normal = normalizeStep(m.Normal, Step{Effort: "high"})
		m.UseMore = normalizeStep(m.UseMore, m.Normal)
		m.Fast = normalizeStep(m.Fast, m.UseMore)
		m.Conserve = normalizeStep(m.Conserve, m.Normal)
		if m.MaxOutputTokens <= 0 {
			m.MaxOutputTokens = 32000
		}
		harnesses := make(map[string]string, len(m.Harnesses))
		for name, model := range m.Harnesses {
			harnesses[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(model)
		}
		m.Harnesses = harnesses
	}
	return out
}

// Validate rejects configurations that cannot produce safe advice.
func (cfg Config) Validate() error {
	r := cfg.Resolve()
	if r.MinRefreshSeconds < 30 || r.MaxRefreshSeconds > 86400 || r.MinRefreshSeconds > r.MaxRefreshSeconds {
		return fmt.Errorf("refresh seconds must satisfy 30 <= min-refresh-seconds <= max-refresh-seconds <= 86400")
	}
	for _, pattern := range r.NeverAdvise {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("never-advise has invalid pattern %q", pattern)
		}
	}
	if err := r.validateNumbers(); err != nil {
		return err
	}
	if r.SafetyFactor < 1 {
		return fmt.Errorf("safety-factor must be at least 1")
	}
	if r.ReservePercent < 0 || r.ReservePercent > 50 {
		return fmt.Errorf("reserve-percent must be between 0 and 50")
	}
	if r.ShortWindowCautionPercent <= 0 || r.ShortWindowBlockPercent > 100 || r.ShortWindowCautionPercent > r.ShortWindowBlockPercent {
		return fmt.Errorf("short-window percentages must satisfy 0 < caution <= block <= 100")
	}
	if r.FastMode != "auto" && r.FastMode != "off" {
		return fmt.Errorf("fast-mode must be auto or off")
	}
	if r.ReuseSeconds < 30 || r.ReuseSeconds > 86400 {
		return fmt.Errorf("reuse-seconds must be between 30 and 86400")
	}
	if r.TrendDays < 1 || r.TrendDays > 28 {
		return fmt.Errorf("trend-days must be between 1 and 28")
	}
	if r.CreditSpendDays <= 0 {
		return fmt.Errorf("credit-spend-days must be positive")
	}
	if r.XHighBelowRatio <= 0 || r.XHighBelowRatio > r.ConserveAboveRatio {
		return fmt.Errorf("ratios must satisfy 0 < xhigh-below-ratio <= conserve-above-ratio")
	}
	if r.FastHorizonHours <= 0 {
		return fmt.Errorf("fast-horizon-hours must be positive")
	}
	if r.OpenCodeUsageURL != "" && !strings.HasPrefix(r.OpenCodeUsageURL, "http://") && !strings.HasPrefix(r.OpenCodeUsageURL, "https://") {
		return fmt.Errorf("opencode-go-usage-url must be an http(s) URL")
	}
	for name, p := range r.Providers {
		if p.CapacityUnits < 0 {
			return fmt.Errorf("providers.%s.capacity-units must not be negative", name)
		}
	}
	names := map[string]bool{}
	for i, m := range r.Models {
		if strings.TrimSpace(m.Name) == "" || names[m.Name] {
			return fmt.Errorf("models[%d] needs a unique name", i)
		}
		names[m.Name] = true
		if len(m.Match) == 0 {
			return fmt.Errorf("models[%d] (%s) needs at least one match pattern", i, m.Name)
		}
		for _, pattern := range m.Match {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("models[%d] (%s) has invalid pattern %q", i, m.Name, pattern)
			}
		}
		for _, step := range m.steps() {
			if effortRank(step.Effort) < 0 {
				return fmt.Errorf("models[%d] (%s) has unknown effort %q", i, m.Name, step.Effort)
			}
			if step.Effort == "max" {
				return fmt.Errorf("models[%d] (%s) must not advise effort max", i, m.Name)
			}
			if step.Mode != ModeStandard && step.Mode != ModeFast && step.Mode != ModePriority {
				return fmt.Errorf("models[%d] (%s) has unknown mode %q", i, m.Name, step.Mode)
			}
		}
	}
	return nil
}

// validateNumbers rejects non-finite or out-of-range numbers, which would
// otherwise reach the forecast and break JSON encoding or budget division.
func (r Config) validateNumbers() error {
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	nonNegative := func(name string, v float64) error {
		if !finite(v) || v < 0 {
			return fmt.Errorf("subagent-advisor %s must be a finite non-negative number", name)
		}
		return nil
	}
	price := func(name string, p *Price) error {
		if p == nil {
			return nil
		}
		for _, v := range []float64{p.Input, p.Output, p.CacheRead} {
			if !finite(v) || v < 0 {
				return fmt.Errorf("subagent-advisor %s must use finite non-negative prices", name)
			}
		}
		if p.Input+p.Output <= 0 {
			return fmt.Errorf("subagent-advisor %s needs a positive input or output price", name)
		}
		return nil
	}
	checks := map[string]float64{
		"surplus-horizon-hours":        r.SurplusHorizonHours,
		"safety-factor":                r.SafetyFactor,
		"reserve-percent":              r.ReservePercent,
		"short-window-caution-percent": r.ShortWindowCautionPercent,
		"short-window-block-percent":   r.ShortWindowBlockPercent,
		"in-flight-request-percent":    r.InFlightRequestPercent,
		"trend-days":                   r.TrendDays,
		"credit-spend-days":            r.CreditSpendDays,
		"xhigh-below-ratio":            r.XHighBelowRatio,
		"conserve-above-ratio":         r.ConserveAboveRatio,
		"fast-horizon-hours":           r.FastHorizonHours,
		"fast-min-expiring-percent":    r.FastMinExpiringPercent,
		"opencode-go-avoid-percent":    r.OpenCodeAvoidPercent,
	}
	for name, v := range checks {
		if err := nonNegative(name, v); err != nil {
			return err
		}
	}
	if r.StaleAfterSeconds < 0 || r.ExpiredAfterSeconds < 0 {
		return fmt.Errorf("subagent-advisor observation ages must not be negative")
	}
	for id, v := range r.AccountCapacityUnits {
		if err := nonNegative("account-capacity-units."+id, v); err != nil {
			return err
		}
	}
	for name, p := range r.Providers {
		if err := nonNegative("providers."+name+".capacity-units", p.CapacityUnits); err != nil {
			return err
		}
		if err := nonNegative("providers."+name+".credits-per-allowance", p.CreditsPerAllowance); err != nil {
			return err
		}
		for plan, v := range p.PlanCapacityUnits {
			if err := nonNegative("providers."+name+".plan-capacity-units."+plan, v); err != nil {
				return err
			}
		}
		if err := price("providers."+name+".default-price", p.DefaultPrice); err != nil {
			return err
		}
	}
	for glob, p := range r.Prices {
		p := p
		if err := price("prices."+glob, &p); err != nil {
			return err
		}
	}
	for _, m := range r.Models {
		if err := price("models."+m.Name+".price", m.Price); err != nil {
			return err
		}
	}
	return nil
}

// Active reports whether advice is enabled.
func (cfg Config) Active() bool { return cfg.Enabled == nil || *cfg.Enabled }

func normalizeProvider(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

func normalizeEffort(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	switch e {
	case "extra-high", "extra_high", "extrahigh", "x-high":
		return "xhigh"
	}
	return e
}

// normalizeStep normalizes a step; an empty effort takes the fallback step.
func normalizeStep(s, fallback Step) Step {
	if strings.TrimSpace(s.Effort) == "" {
		return fallback
	}
	return Step{Effort: normalizeEffort(s.Effort), Mode: normalizeMode(s.Mode)}
}

// steps lists the rule's steps from conserve to fast.
func (m ModelCapability) steps() []Step {
	return []Step{m.Conserve, m.Normal, m.UseMore, m.Fast}
}

func normalizeMode(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "" {
		return ModeStandard
	}
	return m
}

func effortRank(e string) int {
	for i, v := range effortOrder {
		if v == e {
			return i
		}
	}
	return -1
}

func sortEfforts(efforts []string) []string {
	out := make([]string, 0, len(efforts))
	for _, e := range efforts {
		if e != "" && !contains(out, e) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return effortRank(out[i]) < effortRank(out[j]) })
	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// matchModel reports whether id matches any glob, case-insensitively.
func matchModel(patterns []string, id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, pattern := range patterns {
		if ok, _ := path.Match(strings.ToLower(strings.TrimSpace(pattern)), id); ok {
			return true
		}
	}
	return false
}

// priceFor returns the historical cost estimate for a model on a provider.
func (cfg Config) priceFor(provider, model string) (Price, bool) {
	for _, m := range cfg.Models {
		if m.Price != nil && (m.Provider == "" || m.Provider == provider) && matchModel(m.Match, model) {
			return *m.Price, true
		}
	}
	globs := make([]string, 0, len(cfg.Prices))
	for glob := range cfg.Prices {
		globs = append(globs, glob)
	}
	sort.Strings(globs)
	for _, glob := range globs {
		if matchModel([]string{glob}, model) {
			return cfg.Prices[glob], true
		}
	}
	if p, ok := cfg.Providers[provider]; ok && p.DefaultPrice != nil {
		return *p.DefaultPrice, true
	}
	return Price{}, false
}

// cost returns USD-equivalent units for the token counts.
func (p Price) cost(input, output, cacheRead int64) float64 {
	return (float64(input)*p.Input + float64(output)*p.Output + float64(cacheRead)*p.CacheRead) / 1e6
}
