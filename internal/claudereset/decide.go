// Package claudereset decides when to spend Claude subscription reset grants.
//
// A grant only adds value when it lets the fleet serve demand that it could not
// serve otherwise. Allowance that renews unused is lost, and a grant spent early
// only moves consumption from that allowance to the restored one. So a grant is
// spent when the whole fleet is (nearly) out of the allowance it restores and a
// matching request arrived recently. It is spent on the exhausted account whose
// natural reset is furthest away, which leaves the restored allowance the longest
// time to be used. A grant that would otherwise expire unused is spent in its
// last hours on an account with usage to clear, because doing so costs nothing.
package claudereset

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Window names used in a grant's clears list.
const (
	WindowWeekly = "seven_day"
	WindowFable  = "seven_day_overage_included"
)

// Class groups grants by the allowance they restore.
type Class string

const (
	// ClassWeekly clears the account-wide weekly window (and possibly Fable).
	ClassWeekly Class = "weekly"
	// ClassFable clears only the Fable-scoped weekly window.
	ClassFable Class = "fable"
	// ClassUnsupported grants are never spent automatically.
	ClassUnsupported Class = "unsupported"
)

// Triggers recorded on operations.
const (
	TriggerWeeklyNeed = "weekly_need"
	TriggerFableNeed  = "fable_need"
	TriggerExpiry     = "expiry"
)

// Tuning. Usage is measured in percentage points of one account's window, so
// 100 points is one account's full weekly allowance.
const (
	// HeadroomThreshold is the open fleet allowance below which a need
	// trigger spends a grant. Spending on an exhausted account just before
	// the last open allowance runs out loses nothing: routing uses the
	// allowance that renews first, and the restored account renews last.
	HeadroomThreshold = 5.0
	// TargetMinUsed is the usage a target window must reach to be cleared.
	TargetMinUsed = 95.0
	// ImminentReset treats a window that renews this soon as already open, so
	// the fleet waits for the renewal instead of spending a grant.
	ImminentReset = 30 * time.Minute
	// DemandWindow is how recent a quota refusal must be.
	DemandWindow = 15 * time.Minute
	// ExpiryGuard raises the inventory refresh rate before a grant expires.
	ExpiryGuard = 24 * time.Hour
	// ExpiryFinal is the window before a grant's end in which it is spent.
	// Spending later clears more usage; the window leaves room for retries.
	ExpiryFinal = 6 * time.Hour
	// ExpiryCutoff stops claims this close to a grant's end.
	ExpiryCutoff = 2 * time.Minute
	// ExpiryMinClear is the usage an expiring grant must have to clear.
	ExpiryMinClear = 1.0
	// NeedSettle separates need-triggered claims, so the next decision sees the
	// restored allowance.
	NeedSettle = 10 * time.Minute
	// AccountSettle separates claims on one account. A claim also needs an
	// inventory read taken after the previous claim on that account.
	AccountSettle = 10 * time.Minute
	// MaxNeedClaimsPerDay bounds need-triggered claims as a safety net.
	MaxNeedClaimsPerDay = 3
	// StatusFreshFor is the maximum age of usage evidence used to spend.
	StatusFreshFor = 15 * time.Minute
)

// Window is one usage window. Used is a percentage (0-100).
type Window struct {
	Known      bool      `json:"known"`
	Used       float64   `json:"used"`
	ResetAt    time.Time `json:"reset_at,omitzero"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

func (w Window) fresh(now time.Time) bool {
	return w.Known && !w.ObservedAt.IsZero() && !w.ObservedAt.After(now.Add(time.Minute)) && now.Sub(w.ObservedAt) <= StatusFreshFor
}

// Current reports fresh evidence from the window that is running now: not
// renewed since it was observed.
func (w Window) Current(now time.Time) bool {
	return w.fresh(now) && (w.ResetAt.IsZero() || w.ResetAt.After(now))
}

func (w Window) renewsSoon(now time.Time) bool {
	return !w.ResetAt.IsZero() && w.ResetAt.Sub(now) <= ImminentReset
}

// open is the window's unused allowance; renewing windows count as open.
func (w Window) open(now time.Time) float64 {
	if w.renewsSoon(now) {
		return 100
	}
	return w.left()
}

// left is the window's unused allowance right now.
func (w Window) left() float64 {
	return math.Max(0, math.Min(100, 100-w.Used))
}

// Grant is one reset grant as reported by the provider.
type Grant struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	Class       Class     `json:"class"`
	ClearsWeek  bool      `json:"clears_weekly"`
	ClearsFable bool      `json:"clears_fable"`
	ResetsTotal int       `json:"resets_total"`
	ResetsLeft  int       `json:"resets_left"`
	StartsAt    time.Time `json:"starts_at,omitzero"`
	EndsAt      time.Time `json:"ends_at,omitzero"`
	Paused      bool      `json:"paused"`
	UsableNow   bool      `json:"usable_now"`
	// RequiresLimit means the provider accepts the claim only at the limit.
	RequiresLimit bool `json:"requires_limit"`
	// Blocked reports a provider blocking list or unavailable reason.
	Blocked bool `json:"blocked"`
}

// Observation is one account's provider-reported reset state.
type Observation struct {
	ObservedAt       time.Time `json:"observed_at,omitzero"`
	Eligible         bool      `json:"eligible"`
	IneligibleReason string    `json:"ineligible_reason,omitempty"`
	AtLimit          bool      `json:"at_limit"`
	CooldownUntil    time.Time `json:"cooldown_until,omitzero"`
	Exhausted        []string  `json:"exhausted,omitempty"`
	Weekly           Window    `json:"weekly"`
	Fable            Window    `json:"fable"`
	Session          Window    `json:"session"`
	Grants           []Grant   `json:"grants"`
}

// Account is one Claude account as the decision sees it. ObservedAt is the
// grant inventory read; each window carries its own observation time.
type Account struct {
	ID string
	// Healthy accounts can serve: enabled, authenticated and not on hold.
	Healthy bool
	Observation
	// BackoffUntil follows a failed claim on the account.
	BackoffUntil time.Time
	// Pending marks an unresolved claim on the account.
	Pending bool
	// Unrecovered marks a spent reset whose effect no verified read has shown.
	Unrecovered bool
	// LastClaimAt is the latest settled or pending claim on the account.
	LastClaimAt time.Time
}

// Input is everything one decision needs.
type Input struct {
	Now      time.Time
	Accounts []Account
	// ClaudeDemandAt and FableDemandAt are the latest matching requests the
	// fleet refused for quota (HTTP 429): demand it could not serve.
	ClaudeDemandAt time.Time
	FableDemandAt  time.Time
	// LastNeedClaimAt and NeedClaims24h come from need claims, pending or spent.
	LastNeedClaimAt time.Time
	NeedClaims24h   int
}

// Claim is a grant the decision wants spent.
type Claim struct {
	AccountID string `json:"account_id"`
	GrantID   string `json:"grant_id"`
	Class     Class  `json:"class"`
	Trigger   string `json:"trigger"`
	Reason    string `json:"reason"`
}

// Scope states.
const (
	StateOK            = "ok"
	StateWaitingDemand = "waiting_demand"
	StateSettling      = "settling"
	StateCapped        = "capped"
	StateNoGrant       = "no_grant"
	StateSpending      = "spending"
)

// ScopeStatus explains a need trigger.
type ScopeStatus struct {
	Headroom  float64    `json:"headroom"`
	Threshold float64    `json:"threshold"`
	DemandAt  *time.Time `json:"demand_at,omitempty"`
	State     string     `json:"state"`
	Reason    string     `json:"reason"`
}

// Plan is a decision: at most one claim, plus why.
type Plan struct {
	Claim  *Claim      `json:"claim,omitempty"`
	Weekly ScopeStatus `json:"weekly"`
	Fable  ScopeStatus `json:"fable"`
}

// Decide returns the next grant to spend, if any.
func Decide(in Input) Plan {
	plan := Plan{}
	plan.Weekly = needScope(in, in.ClaudeDemandAt, weeklyHeadroom)
	if plan.Weekly.State == StateSpending {
		if claim, reason := weeklyTarget(in); claim != nil {
			plan.Claim = claim
		} else {
			plan.Weekly.State, plan.Weekly.Reason = StateNoGrant, reason
		}
	}
	plan.Fable = needScope(in, in.FableDemandAt, fableHeadroom)
	if plan.Fable.State == StateSpending {
		if plan.Claim != nil {
			plan.Fable.State, plan.Fable.Reason = StateSettling, "a weekly reset is being spent first"
		} else if claim, reason := fableTarget(in); claim != nil {
			plan.Claim = claim
		} else {
			plan.Fable.State, plan.Fable.Reason = StateNoGrant, reason
		}
	}
	if plan.Claim == nil {
		plan.Claim = expiryTarget(in)
	}
	return plan
}

func needScope(in Input, demandAt time.Time, headroom func(Account, time.Time) float64) ScopeStatus {
	now := in.Now
	total := 0.0
	for _, a := range in.Accounts {
		total += headroom(a, now)
	}
	st := ScopeStatus{Headroom: math.Round(total*10) / 10, Threshold: HeadroomThreshold}
	if !demandAt.IsZero() {
		at := demandAt.UTC()
		st.DemandAt = &at
	}
	switch {
	case total >= HeadroomThreshold:
		st.State, st.Reason = StateOK, fmt.Sprintf("%.0f points of allowance are still open across the fleet", total)
	case demandAt.IsZero() || now.Sub(demandAt) > DemandWindow:
		st.State, st.Reason = StateWaitingDemand, "no request was refused for quota in the last 15 minutes"
	case !in.LastNeedClaimAt.IsZero() && !demandAt.After(in.LastNeedClaimAt):
		// One refusal justifies one reset: a new claim needs a refusal after
		// the last one, which may already have met this shortage.
		st.State, st.Reason = StateWaitingDemand, "no request was refused since the last automatic reset"
	case !in.LastNeedClaimAt.IsZero() && now.Sub(in.LastNeedClaimAt) < NeedSettle:
		st.State, st.Reason = StateSettling, "waiting for the last reset to show in usage"
	case in.NeedClaims24h >= MaxNeedClaimsPerDay:
		st.State, st.Reason = StateCapped, "daily safety limit for automatic resets reached"
	default:
		st.State, st.Reason = StateSpending, "the fleet is out of allowance and requests are being refused"
	}
	return st
}

// weeklyHeadroom is the account's open weekly allowance. Accounts that cannot
// serve contribute nothing. Accounts without fresh evidence, or with a claim
// whose outcome is unknown, are assumed open, so neither stale data nor an
// unconfirmed reset ever leads to another spend.
func weeklyHeadroom(a Account, now time.Time) float64 {
	if !a.Healthy {
		return 0
	}
	if unconfirmed(a) || !a.Weekly.fresh(now) {
		return 100
	}
	if sessionBlocked(a, now) {
		return 0
	}
	return a.Weekly.open(now)
}

// fableHeadroom is the account's open Fable allowance. Fable also draws the
// weekly window, so it is bounded by the weekly headroom.
func fableHeadroom(a Account, now time.Time) float64 {
	weekly := weeklyHeadroom(a, now)
	if !a.Healthy || unconfirmed(a) || !a.Fable.fresh(now) {
		return weekly
	}
	return math.Min(weekly, a.Fable.open(now))
}

// unconfirmed reports an account whose last claim may have restored allowance
// that no verified read has shown taking effect (a read right after the claim
// may still show the old usage); it counts as restored, so a reset never
// justifies spending another on stale evidence.
func unconfirmed(a Account) bool {
	return a.Pending || a.Unrecovered || (!a.LastClaimAt.IsZero() && !a.ObservedAt.After(a.LastClaimAt))
}

// sessionBlocked reports a full 5-hour window that keeps the account from
// serving for a while; one renewing soon still counts as open headroom.
func sessionBlocked(a Account, now time.Time) bool {
	return a.Session.fresh(now) && a.Session.Used >= 100 && !a.Session.renewsSoon(now)
}

// SessionBlocked reports a verified full 5-hour window that keeps the account
// from serving for a while (not renewing soon).
func SessionBlocked(a Account, now time.Time) bool { return sessionBlocked(a, now) }

// sessionFull reports a known-full 5-hour window; such an account is never a
// claim target, however soon the window renews.
func sessionFull(a Account, now time.Time) bool {
	return a.Session.fresh(now) && a.Session.Used >= 100 && (a.Session.ResetAt.IsZero() || a.Session.ResetAt.After(now))
}

func exhausted(a Account, window string, w Window) bool {
	if w.Known && w.Used >= TargetMinUsed {
		return true
	}
	for _, name := range a.Exhausted {
		if name == window {
			return true
		}
	}
	return false
}

// Usable reports whether grant g on account a can be claimed now, and why not.
func Usable(a Account, g Grant, now time.Time) (bool, string) {
	switch {
	case !a.Eligible:
		return false, "account_ineligible"
	case !a.CooldownUntil.IsZero() && a.CooldownUntil.After(now):
		return false, "provider_cooldown"
	case g.Class == ClassUnsupported:
		return false, "unsupported_grant"
	case g.ResetsLeft <= 0:
		return false, "no_resets_left"
	case g.Paused:
		return false, "paused"
	case !g.UsableNow:
		return false, "not_usable_now"
	case g.Blocked:
		return false, "blocked"
	case !g.StartsAt.IsZero() && g.StartsAt.After(now):
		return false, "not_started"
	case !g.EndsAt.IsZero() && g.EndsAt.Sub(now) <= ExpiryCutoff:
		return false, "expired"
	case g.RequiresLimit && !a.AtLimit:
		return false, "requires_limit"
	case a.Pending:
		return false, "pending"
	case a.Unrecovered:
		return false, "unrecovered"
	case !a.BackoffUntil.IsZero() && a.BackoffUntil.After(now):
		return false, "backoff"
	case !a.LastClaimAt.IsZero() && (now.Sub(a.LastClaimAt) < AccountSettle || !a.ObservedAt.After(a.LastClaimAt)):
		return false, "settling"
	}
	return true, ""
}

// bestGrant returns the usable grant of class that expires first.
func bestGrant(a Account, class Class, now time.Time) (Grant, bool) {
	var best Grant
	found := false
	for _, g := range a.Grants {
		if g.Class != class {
			continue
		}
		if ok, _ := Usable(a, g, now); !ok {
			continue
		}
		if !found || endsBefore(g, best) {
			best, found = g, true
		}
	}
	return best, found
}

func endsBefore(a, b Grant) bool {
	switch {
	case a.EndsAt.IsZero():
		return false
	case b.EndsAt.IsZero():
		return true
	case !a.EndsAt.Equal(b.EndsAt):
		return a.EndsAt.Before(b.EndsAt)
	}
	return a.ID < b.ID
}

type candidate struct {
	account Account
	grant   Grant
	score   float64
	reset   time.Time
}

func pick(cands []candidate) candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.reset.Equal(b.reset) {
			return a.reset.After(b.reset)
		}
		if endsBefore(a.grant, b.grant) != endsBefore(b.grant, a.grant) {
			return endsBefore(a.grant, b.grant)
		}
		return a.account.ID < b.account.ID
	})
	return cands[0]
}

// weeklyTarget picks the exhausted account whose weekly allowance renews last.
func weeklyTarget(in Input) (*Claim, string) {
	now := in.Now
	var cands []candidate
	for _, a := range in.Accounts {
		if !a.Healthy || !a.Weekly.fresh(now) || sessionFull(a, now) || !exhausted(a, WindowWeekly, a.Weekly) {
			continue
		}
		if a.Weekly.ResetAt.IsZero() || a.Weekly.renewsSoon(now) {
			continue
		}
		g, ok := bestGrant(a, ClassWeekly, now)
		if !ok {
			continue
		}
		cands = append(cands, candidate{account: a, grant: g, reset: a.Weekly.ResetAt})
	}
	if len(cands) == 0 {
		return nil, "no exhausted account has a usable weekly reset"
	}
	c := pick(cands)
	return &Claim{AccountID: c.account.ID, GrantID: c.grant.ID, Class: ClassWeekly, Trigger: TriggerWeeklyNeed,
		Reason: fmt.Sprintf("fleet weekly allowance is used up; this account renews last (%s)", c.reset.UTC().Format(time.RFC3339))}, ""
}

// fableTarget picks a Fable-exhausted account with the most weekly allowance
// left right now, since restored Fable usage still draws the weekly window.
func fableTarget(in Input) (*Claim, string) {
	now := in.Now
	var cands []candidate
	for _, a := range in.Accounts {
		if !a.Healthy || !a.Fable.fresh(now) || !a.Weekly.fresh(now) || sessionFull(a, now) || !exhausted(a, WindowFable, a.Fable) {
			continue
		}
		weekly := a.Weekly.left()
		if weekly < HeadroomThreshold || a.Fable.ResetAt.IsZero() || a.Fable.renewsSoon(now) || a.Weekly.renewsSoon(now) {
			continue
		}
		g, ok := bestGrant(a, ClassFable, now)
		if !ok {
			continue
		}
		cands = append(cands, candidate{account: a, grant: g, score: math.Floor(weekly / 10), reset: a.Fable.ResetAt})
	}
	if len(cands) == 0 {
		return nil, "no Fable-exhausted account with weekly allowance left has a usable Fable reset"
	}
	c := pick(cands)
	return &Claim{AccountID: c.account.ID, GrantID: c.grant.ID, Class: ClassFable, Trigger: TriggerFableNeed,
		Reason: "fleet Fable allowance is used up; this account has the most weekly allowance left"}, ""
}

// cleared returns the usage grant g would clear on a, or false when a cleared
// window lacks fresh evidence or renews before the grant ends (the renewal
// clears it anyway, and usage after it is the better target).
func cleared(a Account, g Grant, now time.Time) (float64, bool) {
	best := 0.0
	for _, item := range []struct {
		clears bool
		w      Window
	}{{g.ClearsWeek, a.Weekly}, {g.ClearsFable, a.Fable}} {
		if !item.clears {
			continue
		}
		if !item.w.fresh(now) || item.w.ResetAt.IsZero() || !item.w.ResetAt.After(g.EndsAt) {
			return 0, false
		}
		best = math.Max(best, item.w.Used)
	}
	return best, true
}

// expiryTarget spends a grant in its last hours when it would otherwise expire
// unused and still clears usage.
func expiryTarget(in Input) *Claim {
	now := in.Now
	var best *candidate
	for _, a := range in.Accounts {
		if !a.Healthy || sessionFull(a, now) {
			continue
		}
		for _, g := range a.Grants {
			if g.EndsAt.IsZero() || g.EndsAt.Sub(now) > ExpiryFinal {
				continue
			}
			if ok, _ := Usable(a, g, now); !ok {
				continue
			}
			used, ok := cleared(a, g, now)
			if !ok || used < ExpiryMinClear {
				continue
			}
			c := candidate{account: a, grant: g, score: used}
			if best == nil || endsBefore(g, best.grant) || (!endsBefore(best.grant, g) && (used > best.score || (used == best.score && a.ID < best.account.ID))) {
				best = &c
			}
		}
	}
	if best == nil {
		return nil
	}
	return &Claim{AccountID: best.account.ID, GrantID: best.grant.ID, Class: best.grant.Class, Trigger: TriggerExpiry,
		Reason: fmt.Sprintf("grant expires %s unused otherwise; clears %.0f%% usage", best.grant.EndsAt.UTC().Format(time.RFC3339), best.score)}
}
