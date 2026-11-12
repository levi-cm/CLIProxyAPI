package subagentadvice

import (
	"context"
	"time"
)

// InventorySource supplies the current accounts, quotas and models. It is
// called for every plan and forecast, so account additions, removals,
// authentication failures and quota changes take effect immediately.
type InventorySource interface {
	Inventory(ctx context.Context) (Inventory, error)
}

type backgroundKey struct{}

// WithBackgroundRefresh marks ctx as the advisor's own background sampler, the
// only caller allowed to read durable history. Advice requests never touch it
// and use what the sampler last read.
func WithBackgroundRefresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// BackgroundRefresh reports whether ctx belongs to the background sampler.
func BackgroundRefresh(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

// InventoryFunc adapts a function to InventorySource.
type InventoryFunc func(ctx context.Context) (Inventory, error)

// Inventory implements InventorySource.
func (f InventoryFunc) Inventory(ctx context.Context) (Inventory, error) { return f(ctx) }

// Inventory is one consistent view of all configured accounts.
type Inventory struct {
	Accounts []Account
	// Models lists the models currently registered for at least one account.
	Models []DiscoveredModel
	// RoutingOverrides maps a provider to an active operator routing override.
	// Slots for such providers are never pinned, so the override keeps control.
	RoutingOverrides map[string]string
	// RoutingAccounts maps a provider to the account a single-account override
	// sends requests to, so unpinned slots reserve against the right account.
	RoutingAccounts map[string]string
}

// Account is one upstream credential. ID is internal and never published.
type Account struct {
	ID       string
	Provider string
	Plan     string
	// Models are the client-visible model IDs this account serves.
	Models []string
	// Canonical maps a visible model ID to the canonical model behind it
	// (prefixes and aliases), so exclusions cannot be bypassed by renaming.
	Canonical map[string]string
	// Blocked is empty when the account can serve requests now; otherwise it
	// names the reason (disabled, auth_error, cooldown, hold, policy_ineligible...).
	Blocked string
	// BlockedUntil is when a temporary block ends; zero when unknown.
	BlockedUntil time.Time
	// ModelBlocks holds per-model blocks keyed by visible model ID.
	ModelBlocks map[string]Block
	// Windows are the latest quota observations.
	Windows []Window
	// ServingOnCredits marks an account whose allowance is exhausted but which
	// stays eligible on its credit balance; it never counts as surplus.
	ServingOnCredits bool
	ActiveRequests   int
	// ReservePercent is an operator reserve for this account.
	ReservePercent float64
	// History holds durable long-window observations from the last day.
	History []Sample
	// Usage holds token totals from the request ledger; nil when unavailable.
	Usage *UsageHistory
}

// Block is a temporary model-level unavailability.
type Block struct {
	Reason string
	Until  time.Time
}

// Window is one observed quota window.
type Window struct {
	// Name identifies the window, for example "7d", "5h" or "7d_oi".
	Name string
	// Long marks the allowance window whose unused part expires at reset.
	Long bool
	// Model scopes the window to matching models (glob); empty applies to all.
	Model           string
	UsedPercent     float64
	ResetAt         time.Time
	DurationSeconds int64
	ObservedAt      time.Time
	// Full marks a window the provider reported as rejected.
	Full bool
}

// Sample is one observation of a long window's utilization.
type Sample struct {
	At          time.Time
	UsedPercent float64
	ResetAt     time.Time
}

// UsageHistory holds token totals for trailing periods.
type UsageHistory struct {
	Last24h []ModelUsage
	Last3h  []ModelUsage
}

// ModelUsage aggregates tokens for one model.
type ModelUsage struct {
	Model           string
	Requests        int64
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

// DiscoveredModel is a model as advertised by the registry.
type DiscoveredModel struct {
	ID       string
	Provider string
	// Levels are the discrete reasoning levels the registry advertises; empty
	// when the model carries no level metadata.
	Levels          []string
	ContextTokens   int
	MaxOutputTokens int
}
