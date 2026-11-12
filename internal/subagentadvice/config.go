// Package subagentadvice forecasts provider quota and turns it into
// deterministic, read-only subagent advice for orchestrators: how freely to
// use subagents and with which model, reasoning effort and mode. It owns
// capability rules and forecasting; it reserves nothing and runs nothing.
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
// service tier. Both spend allowance faster and are selected only for surplus.
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
	// TaskOutputTokens estimates one subagent task's total output at "high" effort (default 40000).
	TaskOutputTokens int `yaml:"task-output-tokens,omitempty" json:"task-output-tokens,omitempty"`
	// TaskInputTokens estimates one subagent task's total input across its requests (default 300000).
	TaskInputTokens int `yaml:"task-input-tokens,omitempty" json:"task-input-tokens,omitempty"`
	// UnmeteredSubagentPercent is the conservative cost of one subagent task, in
	// percentage points of an account whose capacity is unknown (default 2).
	UnmeteredSubagentPercent float64 `yaml:"unmetered-subagent-percent,omitempty" json:"unmetered-subagent-percent,omitempty"`
	// InFlightRequestPercent is the assumed remaining cost of each in-flight request (default 0.05).
	InFlightRequestPercent float64 `yaml:"in-flight-request-percent,omitempty" json:"in-flight-request-percent,omitempty"`
	// FastMode is "auto" (default) or "off".
	FastMode string `yaml:"fast-mode,omitempty" json:"fast-mode,omitempty"`
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
	// provider's cost units (for Claude: Opus 5.5-equivalent USD). Zero means unknown.
	CapacityUnits float64 `yaml:"capacity-units,omitempty" json:"capacity-units,omitempty"`
	// PlanCapacityUnits overrides CapacityUnits by subscription plan (for example "pro").
	PlanCapacityUnits map[string]float64 `yaml:"plan-capacity-units,omitempty" json:"plan-capacity-units,omitempty"`
	// DefaultPrice estimates historical cost for models without a price entry.
	DefaultPrice *Price `yaml:"default-price,omitempty" json:"default-price,omitempty"`
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
	// Efforts are the allowed reasoning efforts.
	Efforts []string `yaml:"efforts" json:"efforts"`
	// DefaultEffort is used without surplus (defaults to the lowest allowed effort).
	DefaultEffort string `yaml:"default-effort,omitempty" json:"default-effort,omitempty"`
	// Modes are the allowed execution modes; standard is always allowed.
	Modes []string `yaml:"modes,omitempty" json:"modes,omitempty"`
	// ModeCostMultiplier is the allowance cost of each non-standard mode (default 2).
	ModeCostMultiplier map[string]float64 `yaml:"mode-cost-multiplier,omitempty" json:"mode-cost-multiplier,omitempty"`
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
		TaskOutputTokens:          40000,
		TaskInputTokens:           300000,
		UnmeteredSubagentPercent:  2,
		InFlightRequestPercent:    0.05,
		FastMode:                  "auto",
		Providers: map[string]ProviderConfig{
			"claude": {
				CapacityUnits: 13000,
				DefaultPrice:  &Price{Input: 4, Output: 20, CacheRead: 0.2},
			},
			"codex":       {},
			"opencode-go": {},
		},
		Models: []ModelCapability{
			{
				Name:               "claude-opus-5.5",
				Match:              []string{"claude-opus-5-5"},
				Provider:           "claude",
				Efforts:            []string{"xhigh"},
				DefaultEffort:      "xhigh",
				Modes:              []string{ModeStandard, ModeFast},
				ModeCostMultiplier: map[string]float64{ModeFast: 2},
				Price:              &Price{Input: 4, Output: 20, CacheRead: 0.2},
				MaxOutputTokens:    64000,
				Harnesses:          map[string]string{"claude-code": "opus", "opencode": ""},
			},
			{
				Name:               "gpt-6.1-sol",
				Match:              []string{"gpt-6.1-sol"},
				Provider:           "codex",
				Efforts:            []string{"high", "xhigh"},
				DefaultEffort:      "high",
				Modes:              []string{ModeStandard, ModePriority},
				ModeCostMultiplier: map[string]float64{ModePriority: 2},
				Price:              &Price{Input: 2.5, Output: 15, CacheRead: 0.25},
				MaxOutputTokens:    32000,
				Harnesses:          map[string]string{"codex": "", "opencode": ""},
			},
			{
				Name:            "deepseek-4.1",
				Match:           []string{"opencode-go/deepseek-v4.1*", "deepseek-v4.1*"},
				Provider:        "opencode-go",
				Efforts:         []string{"high", "max"},
				DefaultEffort:   "high",
				Modes:           []string{ModeStandard},
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
	setInt(&out.TaskOutputTokens, cfg.TaskOutputTokens)
	setInt(&out.TaskInputTokens, cfg.TaskInputTokens)
	setFloat(&out.UnmeteredSubagentPercent, cfg.UnmeteredSubagentPercent)
	setFloat(&out.InFlightRequestPercent, cfg.InFlightRequestPercent)
	if v := strings.TrimSpace(cfg.FastMode); v != "" {
		out.FastMode = strings.ToLower(v)
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
		efforts := make([]string, 0, len(m.Efforts))
		for _, effort := range m.Efforts {
			efforts = append(efforts, normalizeEffort(effort))
		}
		m.Efforts = sortEfforts(efforts)
		m.Match = append([]string(nil), m.Match...)
		m.DefaultEffort = normalizeEffort(m.DefaultEffort)
		if m.DefaultEffort == "" && len(m.Efforts) > 0 {
			m.DefaultEffort = m.Efforts[0]
		}
		modes := []string{ModeStandard}
		for _, mode := range m.Modes {
			mode = strings.ToLower(strings.TrimSpace(mode))
			if mode != ModeStandard && mode != "" && !contains(modes, mode) {
				modes = append(modes, mode)
			}
		}
		m.Modes = modes
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
		if len(m.Efforts) == 0 {
			return fmt.Errorf("models[%d] (%s) needs at least one effort", i, m.Name)
		}
		for _, effort := range m.Efforts {
			if effortRank(effort) < 0 {
				return fmt.Errorf("models[%d] (%s) has unknown effort %q", i, m.Name, effort)
			}
		}
		if !contains(m.Efforts, m.DefaultEffort) {
			return fmt.Errorf("models[%d] (%s) default-effort must be one of its efforts", i, m.Name)
		}
		for _, mode := range m.Modes {
			if mode != ModeStandard && mode != ModeFast && mode != ModePriority {
				return fmt.Errorf("models[%d] (%s) has unknown mode %q", i, m.Name, mode)
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
		"surplus-horizon-hours": r.SurplusHorizonHours, "safety-factor": r.SafetyFactor, "reserve-percent": r.ReservePercent,
		"short-window-caution-percent": r.ShortWindowCautionPercent, "short-window-block-percent": r.ShortWindowBlockPercent,
		"unmetered-subagent-percent": r.UnmeteredSubagentPercent, "in-flight-request-percent": r.InFlightRequestPercent,
	}
	for name, v := range checks {
		if err := nonNegative(name, v); err != nil {
			return err
		}
	}
	if r.UnmeteredSubagentPercent <= 0 {
		return fmt.Errorf("subagent-advisor unmetered-subagent-percent must be positive")
	}
	if r.TaskInputTokens < 0 || r.TaskOutputTokens <= 0 || r.StaleAfterSeconds < 0 || r.ExpiredAfterSeconds < 0 {
		return fmt.Errorf("subagent-advisor task tokens and observation ages must not be negative (task-output-tokens must be positive)")
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
		for mode, v := range m.ModeCostMultiplier {
			if !finite(v) || v <= 0 {
				return fmt.Errorf("subagent-advisor models.%s.mode-cost-multiplier.%s must be a finite positive number", m.Name, mode)
			}
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
