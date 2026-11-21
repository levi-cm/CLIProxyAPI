package claudereset

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func weeklyGrant(id string, ends time.Time) Grant {
	return Grant{ID: id, Class: ClassWeekly, ClearsWeek: true, ClearsFable: true, ResetsTotal: 1, ResetsLeft: 1, UsableNow: true, StartsAt: testNow.Add(-24 * time.Hour), EndsAt: ends}
}

func fableGrant(id string, ends time.Time) Grant {
	g := weeklyGrant(id, ends)
	g.Class, g.ClearsWeek = ClassFable, false
	return g
}

func account(id string, weeklyUsed float64, weeklyReset time.Duration, fableUsed float64, grants ...Grant) Account {
	reset := testNow.Add(weeklyReset)
	observed := testNow.Add(-time.Minute)
	return Account{
		ID:      id,
		Healthy: true,
		Observation: Observation{
			ObservedAt: observed,
			Eligible:   true,
			Weekly:     Window{Known: true, Used: weeklyUsed, ResetAt: reset, ObservedAt: observed},
			Fable:      Window{Known: true, Used: fableUsed, ResetAt: reset, ObservedAt: observed},
			Grants:     grants,
		},
	}
}

func month() time.Time { return testNow.Add(20 * 24 * time.Hour) }

func exhaustedFleet() []Account {
	return []Account{
		account("a", 100, 2*24*time.Hour, 0, weeklyGrant("full", month())),
		account("b", 100, 5*24*time.Hour, 0, weeklyGrant("full", month())),
		account("c", 98, 3*24*time.Hour, 0, weeklyGrant("full", month())),
	}
}

func TestWeeklyNeedSpendsOnAccountRenewingLast(t *testing.T) {
	plan := Decide(Input{Now: testNow, Accounts: exhaustedFleet(), ClaudeDemandAt: testNow.Add(-time.Minute)})
	if plan.Claim == nil || plan.Claim.AccountID != "b" || plan.Claim.GrantID != "full" || plan.Claim.Trigger != TriggerWeeklyNeed {
		t.Fatalf("claim = %+v, want weekly need on b", plan.Claim)
	}
	if plan.Weekly.State != StateSpending || plan.Weekly.Headroom != 2 {
		t.Fatalf("weekly scope = %+v", plan.Weekly)
	}
}

func TestWeeklyNeedWaitsWhileAllowanceIsOpen(t *testing.T) {
	fleet := exhaustedFleet()
	fleet[2].Weekly.Used = 90
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow})
	if plan.Claim != nil || plan.Weekly.State != StateOK {
		t.Fatalf("plan = %+v, want no claim while 10 points are open", plan)
	}
}

func TestWeeklyNeedRequiresRecentDemand(t *testing.T) {
	for _, demand := range []time.Time{{}, testNow.Add(-16 * time.Minute)} {
		plan := Decide(Input{Now: testNow, Accounts: exhaustedFleet(), ClaudeDemandAt: demand})
		if plan.Claim != nil || plan.Weekly.State != StateWaitingDemand {
			t.Fatalf("demand %v: plan = %+v, want waiting for demand", demand, plan)
		}
	}
}

func TestStaleEvidenceNeverSpends(t *testing.T) {
	fleet := exhaustedFleet()
	fleet[0].Weekly.ObservedAt = testNow.Add(-time.Hour)
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow})
	if plan.Claim != nil || plan.Weekly.State != StateOK {
		t.Fatalf("plan = %+v, want stale account treated as open", plan)
	}
	// A fresh weekly reading does not make an old Fable reading fresh.
	fleet[0].Weekly.ObservedAt = testNow.Add(-time.Minute)
	for i := range fleet {
		fleet[i].Weekly.Used, fleet[i].Fable.Used = 50, 100
		fleet[i].Grants = append(fleet[i].Grants, fableGrant("fable", month()))
	}
	fleet[0].Fable.ObservedAt = testNow.Add(-time.Hour)
	plan = Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow, FableDemandAt: testNow})
	if plan.Claim != nil || plan.Fable.State != StateOK {
		t.Fatalf("plan = %+v, want the stale Fable reading treated as open", plan)
	}
}

func TestUnhealthyAccountAddsNoHeadroom(t *testing.T) {
	fleet := exhaustedFleet()
	fleet[2].Weekly.Used = 0
	fleet[2].Healthy = false
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow})
	if plan.Claim == nil || plan.Claim.AccountID != "b" {
		t.Fatalf("plan = %+v, want claim when the open account cannot serve", plan)
	}
}

func TestImminentRenewalCountsAsOpen(t *testing.T) {
	fleet := exhaustedFleet()
	fleet[0].Weekly.ResetAt = testNow.Add(20 * time.Minute)
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow})
	if plan.Claim != nil || plan.Weekly.State != StateOK {
		t.Fatalf("plan = %+v, want no claim right before a natural reset", plan)
	}
}

func TestNeedClaimsSettleAndCap(t *testing.T) {
	in := Input{Now: testNow, Accounts: exhaustedFleet(), ClaudeDemandAt: testNow, LastNeedClaimAt: testNow.Add(-5 * time.Minute)}
	if plan := Decide(in); plan.Claim != nil || plan.Weekly.State != StateSettling {
		t.Fatalf("plan = %+v, want settling", plan)
	}
	in.LastNeedClaimAt, in.NeedClaims24h = testNow.Add(-time.Hour), MaxNeedClaimsPerDay
	if plan := Decide(in); plan.Claim != nil || plan.Weekly.State != StateCapped {
		t.Fatalf("plan = %+v, want capped", plan)
	}
	in.NeedClaims24h = 0
	in.Accounts[1].Pending = true
	if plan := Decide(in); plan.Claim != nil || plan.Weekly.State != StateOK {
		t.Fatalf("plan = %+v, want an unconfirmed reset counted as restored", plan)
	}
}

func TestWeeklyNeedWithoutGrantReportsNoGrant(t *testing.T) {
	fleet := exhaustedFleet()
	for i := range fleet {
		fleet[i].Grants[0].ResetsLeft = 0
	}
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow})
	if plan.Claim != nil || plan.Weekly.State != StateNoGrant {
		t.Fatalf("plan = %+v, want no_grant", plan)
	}
}

func TestFableNeedUsesFableGrantWhereWeeklyIsLeft(t *testing.T) {
	fleet := []Account{
		account("a", 90, 4*24*time.Hour, 100, weeklyGrant("full", month()), fableGrant("fable", month())),
		account("b", 20, 6*24*time.Hour, 100, weeklyGrant("full", month()), fableGrant("fable", month())),
		account("c", 50, 6*24*time.Hour, 100, weeklyGrant("full", month())),
	}
	in := Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow, FableDemandAt: testNow}
	plan := Decide(in)
	if plan.Claim == nil || plan.Claim.AccountID != "b" || plan.Claim.GrantID != "fable" || plan.Claim.Trigger != TriggerFableNeed {
		t.Fatalf("claim = %+v, want Fable grant on b", plan.Claim)
	}
	if plan.Weekly.State != StateOK {
		t.Fatalf("weekly = %+v, want ok", plan.Weekly)
	}
	in.FableDemandAt = time.Time{}
	if plan = Decide(in); plan.Claim != nil || plan.Fable.State != StateWaitingDemand {
		t.Fatalf("plan = %+v, want waiting without Fable demand", plan)
	}
}

func TestFableNeedSkipsAccountWhoseWeeklyIsUsedUp(t *testing.T) {
	fleet := []Account{
		account("a", 100, 20*time.Minute, 100, fableGrant("fable", month())),
		account("b", 100, 4*24*time.Hour, 100, fableGrant("fable", month())),
	}
	fleet[0].Fable.ResetAt = testNow.Add(4 * 24 * time.Hour)
	plan := Decide(Input{Now: testNow, Accounts: fleet, FableDemandAt: testNow})
	if plan.Claim != nil {
		t.Fatalf("claim = %+v, want none: no account has weekly allowance for Fable", plan.Claim)
	}
}

func TestFableNeedNeverSpendsWeeklyGrant(t *testing.T) {
	fleet := []Account{
		account("a", 10, 4*24*time.Hour, 100, weeklyGrant("full", month())),
		account("b", 10, 4*24*time.Hour, 100, weeklyGrant("full", month())),
	}
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow, FableDemandAt: testNow})
	if plan.Claim != nil || plan.Fable.State != StateNoGrant {
		t.Fatalf("plan = %+v, want no Fable claim from weekly grants", plan)
	}
}

func TestExpiringGrantIsSpentInItsLastHours(t *testing.T) {
	soon := testNow.Add(5 * time.Hour)
	fleet := []Account{
		account("a", 30, 3*24*time.Hour, 0, weeklyGrant("full", soon)),
		account("b", 60, 3*24*time.Hour, 0, weeklyGrant("full", soon)),
		account("c", 0, 3*24*time.Hour, 0, weeklyGrant("full", soon)),
	}
	plan := Decide(Input{Now: testNow, Accounts: fleet})
	if plan.Claim == nil || plan.Claim.AccountID != "b" || plan.Claim.Trigger != TriggerExpiry {
		t.Fatalf("claim = %+v, want expiry claim on b (most usage)", plan.Claim)
	}
	fleet[1].Weekly.Used, fleet[0].Weekly.Used = 0, 0
	if plan = Decide(Input{Now: testNow, Accounts: fleet}); plan.Claim != nil {
		t.Fatalf("claim = %+v, want none with nothing to clear", plan.Claim)
	}
	fleet[0].Weekly.Used = 40
	fleet[0].Grants[0].EndsAt = testNow.Add(time.Minute)
	if plan = Decide(Input{Now: testNow, Accounts: fleet}); plan.Claim != nil {
		t.Fatalf("claim = %+v, want none inside the expiry cutoff", plan.Claim)
	}
	fleet[0].Grants[0].EndsAt = testNow.Add(10 * time.Hour)
	if plan = Decide(Input{Now: testNow, Accounts: fleet}); plan.Claim != nil {
		t.Fatalf("claim = %+v, want none before the final hours", plan.Claim)
	}
}

func TestExpiryWaitsForRenewalBeforeTheGrantEnds(t *testing.T) {
	fleet := []Account{account("a", 80, 2*time.Hour, 0, weeklyGrant("full", testNow.Add(5*time.Hour)))}
	if plan := Decide(Input{Now: testNow, Accounts: fleet}); plan.Claim != nil {
		t.Fatalf("claim = %+v, want none: the window renews before the grant ends", plan.Claim)
	}
}

func TestExpiryScoresOnlyClearedWindows(t *testing.T) {
	g := weeklyGrant("weekly-only", testNow.Add(5*time.Hour))
	g.ClearsFable = false
	fleet := []Account{account("a", 0, 3*24*time.Hour, 90, g)}
	if plan := Decide(Input{Now: testNow, Accounts: fleet}); plan.Claim != nil {
		t.Fatalf("claim = %+v, want none: Fable usage is not cleared by a weekly-only grant", plan.Claim)
	}
}

func TestPendingAccountIsQuarantinedOnly(t *testing.T) {
	soon := testNow.Add(5 * time.Hour)
	fleet := []Account{
		account("a", 50, 3*24*time.Hour, 0, weeklyGrant("full", soon)),
		account("b", 40, 3*24*time.Hour, 0, weeklyGrant("full", soon)),
	}
	fleet[0].Pending = true
	plan := Decide(Input{Now: testNow, Accounts: fleet})
	if plan.Claim == nil || plan.Claim.AccountID != "b" {
		t.Fatalf("claim = %+v, want the independent expiry claim on b", plan.Claim)
	}
}

func TestAccountSettleNeedsPostClaimInventory(t *testing.T) {
	a := account("a", 100, 3*24*time.Hour, 0)
	g := weeklyGrant("g", month())
	a.LastClaimAt = testNow.Add(-20 * time.Minute)
	a.ObservedAt = testNow.Add(-30 * time.Minute)
	if ok, reason := Usable(a, g, testNow); ok || reason != "settling" {
		t.Fatalf("usable=%v reason=%q, want settling without a post-claim read", ok, reason)
	}
	a.ObservedAt = testNow.Add(-time.Minute)
	if ok, reason := Usable(a, g, testNow); !ok {
		t.Fatalf("unusable after post-claim read: %s", reason)
	}
}

func TestUsableReasons(t *testing.T) {
	a := account("a", 100, 3*24*time.Hour, 0)
	base := weeklyGrant("g", month())
	cases := map[string]func(*Account, *Grant){
		"account_ineligible": func(a *Account, _ *Grant) { a.Eligible = false },
		"provider_cooldown":  func(a *Account, _ *Grant) { a.CooldownUntil = testNow.Add(time.Hour) },
		"unsupported_grant":  func(_ *Account, g *Grant) { g.Class = ClassUnsupported },
		"no_resets_left":     func(_ *Account, g *Grant) { g.ResetsLeft = 0 },
		"paused":             func(_ *Account, g *Grant) { g.Paused = true },
		"not_usable_now":     func(_ *Account, g *Grant) { g.UsableNow = false },
		"blocked":            func(_ *Account, g *Grant) { g.Blocked = true },
		"not_started":        func(_ *Account, g *Grant) { g.StartsAt = testNow.Add(time.Hour) },
		"expired":            func(_ *Account, g *Grant) { g.EndsAt = testNow },
		"requires_limit":     func(_ *Account, g *Grant) { g.RequiresLimit = true },
		"backoff":            func(a *Account, _ *Grant) { a.BackoffUntil = testNow.Add(time.Minute) },
		"pending":            func(a *Account, _ *Grant) { a.Pending = true },
		"unrecovered":        func(a *Account, _ *Grant) { a.Unrecovered = true },
	}
	for want, mutate := range cases {
		acct, g := a, base
		mutate(&acct, &g)
		if ok, reason := Usable(acct, g, testNow); ok || reason != want {
			t.Fatalf("%s: usable=%v reason=%q", want, ok, reason)
		}
	}
	a.AtLimit = true
	g := base
	g.RequiresLimit = true
	if ok, reason := Usable(a, g, testNow); !ok {
		t.Fatalf("at-limit grant unusable: %s", reason)
	}
}

func TestKnownFullSessionIsNeverATarget(t *testing.T) {
	soon := testNow.Add(5 * time.Hour)
	fleet := []Account{account("a", 60, 3*24*time.Hour, 0, weeklyGrant("full", soon))}
	fleet[0].Session = Window{Known: true, Used: 100, ResetAt: testNow.Add(20 * time.Minute), ObservedAt: testNow}
	if plan := Decide(Input{Now: testNow, Accounts: fleet}); plan.Claim != nil {
		t.Fatalf("claim = %+v, want none while the 5-hour window is full", plan.Claim)
	}
}

func TestSessionBlockedAccountIsNotATarget(t *testing.T) {
	fleet := exhaustedFleet()
	fleet[1].Session = Window{Known: true, Used: 100, ResetAt: testNow.Add(3 * time.Hour), ObservedAt: testNow}
	plan := Decide(Input{Now: testNow, Accounts: fleet, ClaudeDemandAt: testNow})
	if plan.Claim == nil || plan.Claim.AccountID != "c" {
		t.Fatalf("claim = %+v, want c (b's 5-hour window still blocks it)", plan.Claim)
	}
}

func TestClassOf(t *testing.T) {
	cases := []struct {
		clears        []string
		want          Class
		weekly, fable bool
	}{
		{[]string{WindowWeekly, WindowFable}, ClassWeekly, true, true},
		{[]string{WindowWeekly}, ClassWeekly, true, false},
		{[]string{WindowFable}, ClassFable, false, true},
		{[]string{"five_hour"}, ClassUnsupported, false, false},
		{[]string{WindowWeekly, "five_hour"}, ClassUnsupported, false, false},
		{nil, ClassUnsupported, false, false},
	}
	for _, c := range cases {
		got, weekly, fable := ClassOf(c.clears)
		if got != c.want || weekly != c.weekly || fable != c.fable {
			t.Fatalf("ClassOf(%v) = %s %v %v, want %s %v %v", c.clears, got, weekly, fable, c.want, c.weekly, c.fable)
		}
	}
}
