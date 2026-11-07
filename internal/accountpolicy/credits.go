package accountpolicy

import (
	"math"
	"time"
)

// CreditSpending lets an account whose ordinary allowance is exhausted keep
// serving from its provider credit balance down to a protected floor. Upstream
// draws credits only after included usage is exhausted, so this never spends
// credits while allowance remains. Zero numeric fields select the defaults.
type CreditSpending struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// FloorCredits is the protected per-account reserve.
	FloorCredits float64 `json:"floor_credits" yaml:"floor-credits"`
	// MarginCredits absorbs in-flight requests and balance reporting delay.
	MarginCredits float64 `json:"margin_credits" yaml:"margin-credits"`
	// MaxBalanceAgeSeconds bounds how old the observed balance may be (max 600).
	MaxBalanceAgeSeconds int `json:"max_balance_age_seconds" yaml:"max-balance-age-seconds"`
	// AllowanceGuardHours prefers another account whose allowance renews within
	// this horizon, so renewing allowance is not wasted while credits are spent.
	AllowanceGuardHours float64 `json:"allowance_guard_hours" yaml:"allowance-guard-hours"`
	// CreditValueUSD and USDToEUR only drive estimated value displays.
	CreditValueUSD float64 `json:"credit_value_usd" yaml:"credit-value-usd"`
	USDToEUR       float64 `json:"usd_to_eur" yaml:"usd-to-eur"`
}

const (
	defaultCreditFloor          = 7500
	defaultCreditMargin         = 250
	defaultCreditMaxBalanceAge  = 600
	defaultCreditAllowanceGuard = 36
	defaultCreditValueUSD       = 0.04
	defaultCreditUSDToEUR       = 0.87
)

// Effective returns the settings with defaults applied to zero fields.
func (c CreditSpending) Effective() CreditSpending {
	if c.FloorCredits == 0 {
		c.FloorCredits = defaultCreditFloor
	}
	if c.MarginCredits == 0 {
		c.MarginCredits = defaultCreditMargin
	}
	if c.MaxBalanceAgeSeconds == 0 {
		c.MaxBalanceAgeSeconds = defaultCreditMaxBalanceAge
	}
	if c.AllowanceGuardHours == 0 {
		c.AllowanceGuardHours = defaultCreditAllowanceGuard
	}
	if c.CreditValueUSD == 0 {
		c.CreditValueUSD = defaultCreditValueUSD
	}
	if c.USDToEUR == 0 {
		c.USDToEUR = defaultCreditUSDToEUR
	}
	return c
}

func validCreditSpending(c CreditSpending) bool {
	finite := func(v, lo, hi float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi }
	return finite(c.FloorCredits, 0, 10_000_000) && finite(c.MarginCredits, 0, 100_000) &&
		c.MaxBalanceAgeSeconds >= 0 && c.MaxBalanceAgeSeconds <= defaultCreditMaxBalanceAge &&
		finite(c.AllowanceGuardHours, 0, 168) && finite(c.CreditValueUSD, 0, 10) && finite(c.USDToEUR, 0, 10)
}

// MaxCreditUnlock bounds a manual reserve unlock.
const MaxCreditUnlock = 24 * time.Hour

// validCreditUnlocks rejects reserve unlocks ending more than MaxCreditUnlock
// after now. Past values are allowed; they simply mean locked.
func validCreditUnlocks(s Settings, now time.Time) bool {
	for _, c := range s.Accounts {
		if c.CreditReserveUnlockedUntil != nil && c.CreditReserveUnlockedUntil.After(now.Add(MaxCreditUnlock)) {
			return false
		}
	}
	return true
}

// Credit routing states reported to operators.
const (
	CreditStateDisabled     = "disabled"
	CreditStateAllowance    = "allowance"
	CreditStateCredits      = "credits"
	CreditStateFloorReached = "floor_reached"
	CreditStateUnknown      = "unknown"
	CreditStateUnavailable  = "unavailable"
)

// CreditStatus is a read-only view of an account's credit balance and routing state.
type CreditStatus struct {
	State         string     `json:"state"`
	Balance       *float64   `json:"balance"`
	Unlimited     bool       `json:"unlimited"`
	Floor         float64    `json:"floor"`
	Margin        float64    `json:"margin"`
	Spendable     float64    `json:"spendable"`
	Reserved      float64    `json:"reserved"`
	Unlocked      bool       `json:"unlocked"`
	UnlockedUntil *time.Time `json:"unlocked_until"`
	ObservedAt    time.Time  `json:"observed_at"`
	Fresh         bool       `json:"fresh"`
}

// creditCapableStatus excludes authentication, identity, and spend-control
// failures; only quota exhaustion can be bridged with credits.
func creditCapableStatus(status string) bool {
	switch status {
	case "", "ready", "healthy", "quota", "cooldown", "quota_blocked":
		return true
	}
	return false
}

// ordinaryExhausted reports whether only the ordinary allowance blocks the
// model. Model-specific limits are not known to be covered by credits.
func ordinaryExhausted(snapshot Snapshot, model string) bool {
	exhausted := snapshot.Status == "quota_blocked"
	for _, b := range snapshot.Buckets {
		if !applicable(b, model) {
			continue
		}
		blocked := b.Allowed != nil && !*b.Allowed || b.UsedPercent >= 100
		if !blocked {
			continue
		}
		if b.Model != "" || (b.Scope != "ordinary" && b.Scope != "") {
			return false
		}
		exhausted = true
	}
	return exhausted
}

// CreditState derives the credit routing state for a model. It never polls.
func CreditState(snapshot Snapshot, model string, s Settings, now time.Time) CreditStatus {
	c := s.CreditSpending.Effective()
	status := CreditStatus{State: CreditStateDisabled, Balance: snapshot.CreditBalance, Unlimited: snapshot.CreditsUnlimited, Floor: c.FloorCredits, Margin: c.MarginCredits, ObservedAt: snapshot.ObservedAt}
	if until := s.Accounts[snapshot.Identity.CredentialID].CreditReserveUnlockedUntil; until != nil && until.After(now) {
		status.Unlocked = true
		at := until.UTC()
		status.UnlockedUntil = &at
		status.Floor = 0
	}
	// Only a missing reset-credit inventory leaves the usage balance trustworthy.
	status.Fresh = fresh(snapshot.ObservedAt, now, c.MaxBalanceAgeSeconds) && (snapshot.LastError == "" || snapshot.LastError == "credit_inventory_unsupported")
	if snapshot.CreditBalance != nil {
		balance := *snapshot.CreditBalance
		status.Spendable = math.Max(0, balance-status.Floor-c.MarginCredits)
		status.Reserved = math.Min(math.Max(0, balance), c.FloorCredits)
	}
	if !s.CreditSpending.Enabled || snapshot.Identity.Provider != "codex" {
		return status
	}
	switch {
	case !creditCapableStatus(snapshot.Status):
		status.State = CreditStateUnavailable
	case !ordinaryExhausted(snapshot, model):
		status.State = CreditStateAllowance
	case !status.Fresh || (snapshot.CreditBalance == nil && !snapshot.CreditsUnlimited):
		status.State = CreditStateUnknown
	case snapshot.HasCredits != nil && !*snapshot.HasCredits && !snapshot.CreditsUnlimited:
		status.State = CreditStateFloorReached
	case snapshot.CreditsUnlimited || *snapshot.CreditBalance-status.Floor > c.MarginCredits:
		status.State = CreditStateCredits
	default:
		status.State = CreditStateFloorReached
	}
	return status
}

// creditEvaluation keeps an exhausted account eligible while its balance stays
// above the floor. Its deadline is the guard horizon, which ranks it before
// allowance that renews later and after allowance that would otherwise be lost.
func creditEvaluation(snapshot Snapshot, model string, s Settings, now time.Time) (Evaluation, bool) {
	if CreditState(snapshot, model, s, now).State != CreditStateCredits {
		return Evaluation{}, false
	}
	guard := time.Duration(s.CreditSpending.Effective().AllowanceGuardHours * float64(time.Hour))
	return Evaluation{Eligible: true, Known: true, Credits: true, Deadline: now.Add(guard), Reason: "credit_balance"}, true
}

type creditSample struct {
	at      time.Time
	balance float64
}

const (
	creditSampleWindow  = 2 * time.Hour
	creditSampleMinSpan = 15 * time.Minute
	creditSampleLimit   = 512
)

// recordCreditSampleLocked keeps a short in-memory balance history for burn
// rate estimates. It is intentionally not persisted.
func (s *Service) recordCreditSampleLocked(id string, snapshot Snapshot) {
	if snapshot.CreditBalance == nil || snapshot.ObservedAt.IsZero() {
		return
	}
	if s.creditSamples == nil {
		s.creditSamples = map[string][]creditSample{}
	}
	samples := append(s.creditSamples[id], creditSample{at: snapshot.ObservedAt, balance: *snapshot.CreditBalance})
	cut := 0
	for cut < len(samples) && (snapshot.ObservedAt.Sub(samples[cut].at) > creditSampleWindow || len(samples)-cut > creditSampleLimit) {
		cut++
	}
	s.creditSamples[id] = append([]creditSample(nil), samples[cut:]...)
}

// CreditBurnPerHour estimates credits spent per hour from recent balance
// observations. Balance increases are ignored. ok is false until enough
// history exists.
func (s *Service) CreditBurnPerHour(id string) (perHour float64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	samples := s.creditSamples[id]
	if len(samples) < 2 {
		return 0, false
	}
	span := samples[len(samples)-1].at.Sub(samples[0].at)
	if span < creditSampleMinSpan {
		return 0, false
	}
	spent := 0.0
	for i := 1; i < len(samples); i++ {
		spent += math.Max(0, samples[i-1].balance-samples[i].balance)
	}
	return spent / span.Hours(), true
}
