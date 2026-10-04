// Package accountpolicy implements optional account deadline routing and durable reset automation.
package accountpolicy

import (
	"context"
	"time"
)

type Settings struct {
	Enabled            bool                      `json:"enabled" yaml:"enabled"`
	Mode               string                    `json:"mode" yaml:"mode"`
	Automation         string                    `json:"automation" yaml:"automation"`
	Fallback           string                    `json:"fallback" yaml:"fallback"`
	Affinity           string                    `json:"affinity" yaml:"affinity"`
	StateDir           string                    `json:"state_dir" yaml:"state-dir"`
	TimeZone           string                    `json:"time_zone" yaml:"time-zone"`
	ExpiryGuardSeconds int                       `json:"expiry_guard_seconds" yaml:"expiry-guard-seconds"`
	FreshnessSeconds   int                       `json:"freshness_seconds" yaml:"freshness-seconds"`
	SavedCreditReserve int                       `json:"saved_credit_reserve" yaml:"saved-credit-reserve"`
	ReadOnly           bool                      `json:"read_only" yaml:"read-only"`
	ForceAccount       string                    `json:"force_account" yaml:"force-account"`
	CreditTypes        []string                  `json:"credit_types" yaml:"credit-types"`
	Accounts           map[string]AccountControl `json:"accounts" yaml:"accounts"`
}
type AccountControl struct {
	Hold           bool    `json:"hold" yaml:"hold"`
	ReservePercent float64 `json:"reserve_percent" yaml:"reserve-percent"`
}
type Identity struct {
	CredentialID string `json:"credential_id"`
	AccountID    string `json:"account_id"`
	WorkspaceID  string `json:"workspace_id"`
	Provider     string `json:"provider"`
	Alias        string `json:"alias"`
	Generation   uint64 `json:"generation"`
}
type Bucket struct {
	Scope           string    `json:"scope"`
	Model           string    `json:"model"`
	DurationSeconds int64     `json:"duration_seconds"`
	UsedPercent     float64   `json:"used_percent"`
	Allowed         *bool     `json:"allowed"`
	ResetAt         time.Time `json:"reset_at"`
	ObservedAt      time.Time `json:"observed_at"`
	Source          string    `json:"source"`
}
type Credit struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Title        string     `json:"title"`
	GrantedAt    time.Time  `json:"granted_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	DetailsKnown bool       `json:"details_known"`
	Scopes       []string   `json:"scopes"`
}
type Snapshot struct {
	Identity            Identity  `json:"identity"`
	Version             uint64    `json:"version"`
	Plan                string    `json:"plan"`
	Status              string    `json:"status"`
	Eligible            bool      `json:"eligible"`
	Buckets             []Bucket  `json:"buckets"`
	Credits             []Credit  `json:"credits"`
	AvailableCredits    int       `json:"available_credits"`
	InventoryComplete   bool      `json:"inventory_complete"`
	ObservedAt          time.Time `json:"observed_at"`
	InventoryObservedAt time.Time `json:"inventory_observed_at"`
	LastError           string    `json:"last_error"`
	WritesDisabled      bool      `json:"writes_disabled"`
	ActiveRequests      int       `json:"active_requests"`
	ActiveBindings      int       `json:"active_bindings"`
	Transport           string    `json:"transport"`
}
type Evaluation struct {
	Eligible bool      `json:"eligible"`
	Known    bool      `json:"known"`
	Deadline time.Time `json:"deadline"`
	Reason   string    `json:"reason"`
}
type Decision struct {
	CredentialID string    `json:"credential_id"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	Reason       string    `json:"reason"`
	Deadline     time.Time `json:"deadline"`
	At           time.Time `json:"at"`
	Fallback     bool      `json:"fallback"`
}
type Operation struct {
	ID           string    `json:"id"`
	RequestID    string    `json:"request_id"`
	CredentialID string    `json:"credential_id"`
	AccountID    string    `json:"account_id"`
	WorkspaceID  string    `json:"workspace_id"`
	CreditID     string    `json:"credit_id"`
	State        string    `json:"state"`
	Result       string    `json:"result"`
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Before       *Snapshot `json:"before"`
	After        *Snapshot `json:"after"`
}
type Schedule struct {
	ID           string    `json:"id"`
	CredentialID string    `json:"credential_id"`
	CreditID     string    `json:"credit_id"`
	At           time.Time `json:"at"`
}
type ConsumeResult struct {
	Code         string `json:"code"`
	WindowsReset int    `json:"windows_reset"`
}
type Provider interface {
	Discover(context.Context, Identity) (Snapshot, error)
	Consume(context.Context, Identity, string, string) (ConsumeResult, error)
}
type Options struct {
	Settings     Settings
	Provider     Provider
	Accounts     func() []Identity
	Recover      func(context.Context, Snapshot, Snapshot) error
	Now          func() time.Time
	HasDemand    func(string) bool
	IsIdle       func(string) bool
	AcquireReset func(context.Context, string) (func(), error)
}
