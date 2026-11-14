package subagentadvice

import (
	"fmt"
	"time"
)

// Response schemas. Clients reject unknown versions.
const (
	Schema         = "cliproxy.subagent-advice"
	ForecastSchema = "cliproxy.subagent-forecast"
	// SchemaVersion 3 replaced the agent counter and room with a posture per
	// provider from its usage trend against an even daily budget.
	SchemaVersion = 3
)

// Advice statuses.
const (
	StatusReady       = "ready"
	StatusDegraded    = "degraded"
	StatusUnavailable = "unavailable"
)

// Provider postures. Normal advises the normal step (high), use_more the
// use-more step (extra high, plus fast/priority for allowance that expires
// unused soon), conserve the conserve step and fewer subagents, and avoid
// means do not start subagents on the provider now.
const (
	PostureNormal   = "normal"
	PostureUseMore  = "use_more"
	PostureConserve = "conserve"
	PostureAvoid    = "avoid"
)

// Request narrows one advice computation.
type Request struct {
	// ContextTokens is the largest single prompt a subagent needs; models
	// whose context cannot hold it plus their output budget are skipped.
	// Zero skips the check.
	ContextTokens int
}

// Advice is the read-only answer to "which models, efforts and modes should
// subagents use now". It never gives a number of agents; the postures are
// shared by every orchestrator and nothing is reserved.
type Advice struct {
	Schema              string            `json:"schema"`
	SchemaVersion       int               `json:"schema_version"`
	Status              string            `json:"status"`
	GeneratedAt         time.Time         `json:"generated_at"`
	ValidUntil          time.Time         `json:"valid_until"`
	RefreshAfterSeconds int               `json:"refresh_after_seconds"`
	RefreshReason       string            `json:"refresh_reason"`
	Postures            []ProviderPosture `json:"postures"`
	Models              []ModelAdvice     `json:"models"`
	Summary             string            `json:"summary"`
	Warnings            []string          `json:"warnings"`
	RetryAfterSeconds   *int              `json:"retry_after_seconds,omitempty"`
	Freshness           Freshness         `json:"freshness"`
	Providers           []ProviderSummary `json:"providers"`
}

// ProviderPosture is how orchestrators should use one provider now, in
// display order: normal and use_more first, then conserve, then avoid.
type ProviderPosture struct {
	Provider string `json:"provider"`
	Posture  string `json:"posture"`
	Reason   string `json:"reason"`
	// Trend is the provider pool's usage trend against its budget; nil for
	// providers without one (OpenCode Go).
	Trend *Trend `json:"trend,omitempty"`
}

// Trend compares a provider pool's average daily use over the last days with
// an even daily budget. Percentages are of one account's long-window
// allowance; both are rough estimates.
type Trend struct {
	// Days is the configured trend window; CoveredDays how much of it the
	// history covers.
	Days        float64 `json:"days"`
	CoveredDays float64 `json:"covered_days"`
	// Basis is allowance_history (durable quota observations),
	// request_ledger (priced request tokens) or mixed.
	Basis string `json:"basis,omitempty"`
	// PercentPerDay is nil while the trend is unknown (under a day of history).
	PercentPerDay *float64 `json:"percent_per_day"`
	Unknown       string   `json:"unknown_reason,omitempty"`
	// Accounts and PeriodDays give the allowance budget: accounts x 100% /
	// period days.
	Accounts                     int     `json:"accounts"`
	PeriodDays                   float64 `json:"period_days"`
	AllowanceBudgetPercentPerDay float64 `json:"allowance_budget_percent_per_day"`
	// CreditsPercent is credits above the protected floors, in percent of one
	// account, spread over CreditSpendDays while routing spends credits.
	CreditsPercent      float64 `json:"credits_percent,omitempty"`
	CreditSpendDays     float64 `json:"credit_spend_days,omitempty"`
	BudgetPercentPerDay float64 `json:"budget_percent_per_day"`
	// Ratio is the trend over the budget; nil while the trend is unknown.
	Ratio *float64 `json:"ratio"`
	// ExpiringSoonPercent is allowance that would expire unused within the
	// fast horizon at the trend; nil while the trend is unknown.
	ExpiringSoonPercent *float64 `json:"expiring_soon_percent,omitempty"`
	ExpiringWithinHours float64  `json:"expiring_within_hours,omitempty"`
}

// ModelAdvice is how subagents should use one model, in preference order.
type ModelAdvice struct {
	Capability       string            `json:"capability"`
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`
	ReasoningEffort  string            `json:"reasoning_effort"`
	Mode             string            `json:"mode"`
	Posture          string            `json:"posture"`
	Harnesses        map[string]string `json:"harnesses"`
	CapabilitySource string            `json:"capability_source"`
	Reason           string            `json:"reason"`
}

// Freshness describes the age of the quota evidence behind a response.
type Freshness struct {
	InventoryAt              time.Time  `json:"inventory_at"`
	OldestObservationAt      *time.Time `json:"oldest_observation_at,omitempty"`
	MaxObservationAgeSeconds int        `json:"max_observation_age_seconds"`
	StaleAccounts            int        `json:"stale_accounts"`
	UnmeteredAccounts        int        `json:"unmetered_accounts"`
}

// ProviderSummary is the per-provider forecast behind a response.
type ProviderSummary struct {
	Provider           string     `json:"provider"`
	State              string     `json:"state"`
	Accounts           int        `json:"accounts"`
	UsableAccounts     int        `json:"usable_accounts"`
	NextResetAt        *time.Time `json:"next_reset_at,omitempty"`
	UsableSurplusUnits float64    `json:"usable_surplus_units"`
	Confidence         float64    `json:"confidence"`
	Note               string     `json:"note,omitempty"`
	// Combined treats all metered accounts of the provider as one account.
	Combined *CombinedAllowance `json:"combined,omitempty"`
	// Allowance lists each metered account's weekly (or monthly) allowance,
	// earliest reset first, without identifying accounts.
	Allowance []AllowanceWindow `json:"allowance,omitempty"`
	// OpenCode is OpenCode Go's quota from the usage tracker.
	OpenCode *OpenCodeUsage `json:"opencode,omitempty"`
}

// CombinedAllowance is a provider's allowance as if all its metered accounts
// were one account; percentages are of their combined capacity.
type CombinedAllowance struct {
	Period   string `json:"period"`
	Accounts int    `json:"accounts"`
	// LeftPercent is what usable accounts hold now.
	LeftPercent float64 `json:"left_percent"`
	// Refills are the upcoming resets and what each adds back.
	Refills []Refill `json:"refills"`
	// ProjectedUnusedPercent is the allowance projected to expire unused at
	// resets within the surplus horizon; nil when the pace is unknown.
	ProjectedUnusedPercent *float64 `json:"projected_unused_percent,omitempty"`
	// Credits sums the credit balances above the protected floors.
	Credits *CombinedCredits `json:"credits,omitempty"`
	// ResetGrants counts available full-reset grants, which the reset
	// automation redeems; ResetGrantsExpireAt is the earliest expiry.
	ResetGrants         int        `json:"reset_grants,omitempty"`
	ResetGrantsExpireAt *time.Time `json:"reset_grants_expire_at,omitempty"`
}

// CombinedCredits is a provider's credit balance above the protected floors.
type CombinedCredits struct {
	Spendable float64 `json:"spendable"`
	// FloorPerAccount is the protected reserve each account keeps.
	FloorPerAccount float64 `json:"floor_per_account"`
	// AddPercent is the spendable balance in percent of the combined
	// allowance; zero when the provider sets no credits-per-allowance.
	AddPercent float64 `json:"add_percent"`
	// Enabled reports whether routing spends credits at all.
	Enabled bool `json:"enabled"`
}

// Refill is one account reset in a combined allowance.
type Refill struct {
	At         time.Time `json:"at"`
	AddPercent float64   `json:"add_percent"`
}

// AllowanceWindow is one account's long allowance window.
type AllowanceWindow struct {
	// Period is "weekly" or "monthly".
	Period      string     `json:"period"`
	Usable      bool       `json:"usable"`
	LeftPercent float64    `json:"left_percent"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

// Forecast is the diagnostic view of the evidence behind advice.
type Forecast struct {
	Schema        string            `json:"schema"`
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Providers     []ProviderSummary `json:"providers"`
	Accounts      []AccountForecast `json:"accounts"`
	Models        []ModelForecast   `json:"models"`
	Freshness     Freshness         `json:"freshness"`
}

// AccountForecast is one account's forecast. Accounts are identified only by
// a hashed reference.
type AccountForecast struct {
	AccountRef               string     `json:"account_ref"`
	Provider                 string     `json:"provider"`
	Plan                     string     `json:"plan,omitempty"`
	Usable                   bool       `json:"usable"`
	BlockedReason            string     `json:"blocked_reason,omitempty"`
	AvailableAt              *time.Time `json:"available_at,omitempty"`
	Metered                  bool       `json:"metered"`
	MeterNote                string     `json:"meter_note,omitempty"`
	CapacityUnits            float64    `json:"capacity_units"`
	CapacityBasis            string     `json:"capacity_basis"`
	UsedPercent              *float64   `json:"used_percent,omitempty"`
	ResetAt                  *time.Time `json:"reset_at,omitempty"`
	ObservedAt               *time.Time `json:"observed_at,omitempty"`
	ObservationAgeSeconds    int        `json:"observation_age_seconds,omitempty"`
	Stale                    bool       `json:"stale"`
	PacePercentPerHour       *float64   `json:"pace_percent_per_hour,omitempty"`
	PaceBasis                string     `json:"pace_basis"`
	PaceConfidence           float64    `json:"pace_confidence"`
	ProjectedLeftoverPercent *float64   `json:"projected_leftover_percent,omitempty"`
	UsableSurplusUnits       float64    `json:"usable_surplus_units"`
	ActiveRequests           int        `json:"active_requests"`
	Models                   []string   `json:"models"`
}

// ModelForecast describes how one capability rule resolved.
type ModelForecast struct {
	Capability       string   `json:"capability"`
	Provider         string   `json:"provider,omitempty"`
	Model            string   `json:"model,omitempty"`
	State            string   `json:"state"`
	Reason           string   `json:"reason,omitempty"`
	Efforts          []string `json:"efforts"`
	Modes            []string `json:"modes"`
	CapabilitySource string   `json:"capability_source,omitempty"`
	EligibleAccounts int      `json:"eligible_accounts"`
}

// Error is a typed failure with an HTTP status.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errorf(status int, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Status: status}
}
