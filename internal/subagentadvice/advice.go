package subagentadvice

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/fleetactivity"
)

// Response schemas. Clients reject unknown versions.
const (
	Schema         = "cliproxy.subagent-advice"
	ForecastSchema = "cliproxy.subagent-forecast"
	// SchemaVersion 2 replaced per-model task budgets with a fleet-wide rate,
	// shared room for more agents and effort ladders.
	SchemaVersion = 2
)

// Advice statuses.
const (
	StatusReady       = "ready"
	StatusDegraded    = "degraded"
	StatusUnavailable = "unavailable"
)

// Bases explain why a model was advised.
const (
	// KindRoom: the fleet runs below its target rate; more agents fit.
	KindRoom = "room"
	// KindHold: the fleet already uses its target rate; start no more.
	KindHold = "hold"
	// KindUnmetered: no quota telemetry; budget unknown.
	KindUnmetered = "unmetered"
	// KindConserve: the fleet uses allowance faster than it lasts.
	KindConserve = "conserve"
)

// Request narrows one advice computation.
type Request struct {
	// ContextTokens is the largest single prompt a subagent needs; models
	// whose context cannot hold it plus their output budget are skipped.
	// Zero skips the check.
	ContextTokens int
}

// Advice is the read-only answer to "which models should subagents use now,
// and how many more agents fit". The room is shared by every orchestrator;
// nothing is reserved or capped.
type Advice struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Posture       string `json:"posture"`
	// RoomAgents is how many more agents the whole fleet can start, summed
	// over the providers of the advised models.
	RoomAgents int `json:"room_agents"`
	// Fleet counts the agents working through the proxy right now.
	Fleet               fleetactivity.Snapshot `json:"fleet"`
	GeneratedAt         time.Time              `json:"generated_at"`
	ValidUntil          time.Time              `json:"valid_until"`
	RefreshAfterSeconds int                    `json:"refresh_after_seconds"`
	RefreshReason       string                 `json:"refresh_reason"`
	Models              []ModelAdvice          `json:"models"`
	Summary             string                 `json:"summary"`
	Warnings            []string               `json:"warnings"`
	RetryAfterSeconds   *int                   `json:"retry_after_seconds,omitempty"`
	Freshness           Freshness              `json:"freshness"`
	Providers           []ProviderSummary      `json:"providers"`
}

// ModelAdvice is how subagents should use one model, in preference order.
type ModelAdvice struct {
	Capability      string `json:"capability"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	Mode            string `json:"mode"`
	Basis           string `json:"basis"`
	// Step is the 1-based ladder step advised, of Steps.
	Step  int `json:"step"`
	Steps int `json:"steps"`
	// RoomAgents is the provider's room for more agents across all
	// orchestrators; nil when unknown.
	RoomAgents       *int              `json:"room_agents,omitempty"`
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
	// Fleet is the pool's usage rate against its target.
	Fleet *FleetRate `json:"fleet,omitempty"`
	// OpenCode is OpenCode Go's quota from the usage tracker.
	OpenCode *OpenCodeUsage `json:"opencode,omitempty"`
}

// FleetRate compares a provider pool's recent use with the rate that would
// use every account's remaining allowance by its reset. Rates are percentage
// points of one account per hour.
type FleetRate struct {
	State string `json:"state"`
	// Accounts counts the accounts whose allowance can still be used.
	Accounts int `json:"accounts"`
	// NeedPercentPerHour uses every account's remaining allowance by its
	// reset (plus credits above the floors spread over the surplus horizon).
	NeedPercentPerHour float64 `json:"need_percent_per_hour"`
	// CreditsPercentPerHour is the credit part of the need.
	CreditsPercentPerHour float64 `json:"credits_percent_per_hour,omitempty"`
	// TargetPercentPerHour is the need times target-fraction.
	TargetPercentPerHour float64 `json:"target_percent_per_hour"`
	// RecentPercentPerHour is the fleet's use over the last 3 hours; its basis
	// is observed, ledger or estimated (agents times the per-agent estimate).
	RecentPercentPerHour float64 `json:"recent_percent_per_hour"`
	RecentBasis          string  `json:"recent_basis"`
	Agents               int     `json:"agents"`
	// PerAgentPercentPerHour is recent use per working agent ("measured"),
	// the last such rate within a day while the fleet is idle
	// ("last_measured"), or the per-task estimate turned into a rate
	// ("task_estimate").
	PerAgentPercentPerHour float64 `json:"per_agent_percent_per_hour"`
	PerAgentBasis          string  `json:"per_agent_basis"`
	RoomAgents             int     `json:"room_agents"`
	// ExpiringSoonPercent would still expire unused within the fast horizon
	// with the fleet at the target; above fast-min-percent it unlocks the
	// fast/priority step.
	ExpiringSoonPercent float64 `json:"expiring_soon_percent"`
	TimeSensitive       bool    `json:"time_sensitive"`
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
