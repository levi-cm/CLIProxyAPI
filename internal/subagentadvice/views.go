package subagentadvice

import (
	"math"
	"sort"
)

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (f *forecaster) accountView(st *accountState) AccountForecast {
	v := AccountForecast{AccountRef: st.ref, Provider: st.acct.Provider, Plan: st.acct.Plan, Usable: st.blocked == "", BlockedReason: st.blocked, Metered: st.metered, MeterNote: st.meterReason, CapacityUnits: round(st.capacity, 2), CapacityBasis: st.capacityBasis, Stale: st.stale, PaceBasis: st.paceBasis, PaceConfidence: round(st.paceConfidence, 2), UsableSurplusUnits: round(st.surplusUnits, 2), ActiveRequests: st.acct.ActiveRequests, Models: append([]string{}, st.acct.Models...)}
	sort.Strings(v.Models)
	if !st.blockedUntil.IsZero() && st.blocked != "" {
		at := st.blockedUntil
		v.AvailableAt = &at
	}
	if st.metered {
		used := round(st.used, 2)
		v.UsedPercent = &used
		reset, observed := st.long.ResetAt, st.long.ObservedAt
		if !reset.IsZero() {
			v.ResetAt = &reset
		}
		v.ObservedAt = &observed
		v.ObservationAgeSeconds = int(st.age.Seconds())
		// The pool simulation's leftover at this account's reset, after the
		// router drains demand from the earliest-resetting accounts.
		if pool := f.pools[st.acct.Provider]; pool != nil && pool.demandKnown && !reset.IsZero() {
			leftover := round(st.wasteUnits/st.capacity*100, 2)
			v.ProjectedLeftoverPercent = &leftover
		}
	}
	if st.paceKnown {
		pace := round(st.pace, 3)
		v.PacePercentPerHour = &pace
	}
	return v
}

func (f *forecaster) freshness(inv Inventory) Freshness {
	out := Freshness{InventoryAt: f.now}
	for _, st := range f.accounts {
		if !st.metered {
			out.UnmeteredAccounts++
			continue
		}
		if st.stale {
			out.StaleAccounts++
		}
		observed := st.long.ObservedAt
		if out.OldestObservationAt == nil || observed.Before(*out.OldestObservationAt) {
			at := observed
			out.OldestObservationAt = &at
		}
		if age := int(st.age.Seconds()); age > out.MaxObservationAgeSeconds {
			out.MaxObservationAgeSeconds = age
		}
	}
	return out
}

func (f *forecaster) providerSummaries() []ProviderSummary {
	byProvider := map[string]*ProviderSummary{}
	var names []string
	for _, st := range f.accounts {
		p := byProvider[st.acct.Provider]
		if p == nil {
			p = &ProviderSummary{Provider: st.acct.Provider, State: "unmetered"}
			byProvider[st.acct.Provider] = p
			names = append(names, st.acct.Provider)
		}
		p.Accounts++
		if st.blocked == "" {
			p.UsableAccounts++
		}
		if st.metered {
			w := AllowanceWindow{Period: allowancePeriod(st.long), Usable: st.blocked == "", LeftPercent: round(math.Max(0, 100-st.used), 1)}
			if !st.long.ResetAt.IsZero() {
				at := st.long.ResetAt
				w.ResetAt = &at
			}
			p.Allowance = append(p.Allowance, w)
		}
	}
	sort.Strings(names)
	out := make([]ProviderSummary, 0, len(names))
	for _, name := range names {
		p := byProvider[name]
		p.Combined = f.combined(name)
		sort.SliceStable(p.Allowance, func(i, j int) bool {
			a, b := p.Allowance[i].ResetAt, p.Allowance[j].ResetAt
			if (a == nil) != (b == nil) {
				return a != nil
			}
			return a != nil && a.Before(*b)
		})
		if pool := f.pools[name]; pool != nil {
			p.UsableSurplusUnits = round(pool.surplusUnits, 2)
			p.Confidence = round(pool.confidence, 2)
			if !pool.nextReset.IsZero() {
				at := pool.nextReset
				p.NextResetAt = &at
			}
			switch {
			case p.UsableAccounts == 0:
				p.State = "unavailable"
			case !pool.demandKnown:
				p.State, p.Note = "unknown_pace", "usage pace unknown; surplus is not claimed"
			case p.UsableSurplusUnits > 0:
				p.State = "surplus"
			case pool.shortfallBase > 0:
				p.State, p.Note = "tight", "forecast demand exceeds the allowance before the next refill"
			default:
				p.State = "normal"
			}
		} else if p.UsableAccounts == 0 {
			p.State = "unavailable"
		}
		out = append(out, *p)
	}
	return out
}

// allowancePeriod names a long window by its length.
func allowancePeriod(w Window) string {
	if w.DurationSeconds >= 20*24*3600 {
		return "monthly"
	}
	return "weekly"
}

// combined sums a provider's metered accounts into one allowance, weighting
// each account by its capacity.
func (f *forecaster) combined(provider string) *CombinedAllowance {
	var accounts []*accountState
	total := 0.0
	for _, st := range f.accounts {
		if st.acct.Provider == provider && st.metered {
			accounts = append(accounts, st)
			total += st.capacity
		}
	}
	if len(accounts) == 0 || total <= 0 {
		return nil
	}
	c := &CombinedAllowance{Period: allowancePeriod(accounts[0].long), Accounts: len(accounts), Refills: []Refill{}}
	left := 0.0
	for _, st := range accounts {
		if st.blocked == "" && !st.acct.ServingOnCredits {
			left += math.Max(0, 100-st.used) * st.capacity
		}
		if allowancePeriod(st.long) != c.Period {
			c.Period = "weekly/monthly"
		}
		if add := st.used * st.capacity / total; !st.long.ResetAt.IsZero() && add >= 0.5 {
			c.Refills = append(c.Refills, Refill{At: st.long.ResetAt, AddPercent: round(add, 0)})
		}
	}
	c.LeftPercent = round(left/total, 0)
	sort.SliceStable(c.Refills, func(i, j int) bool { return c.Refills[i].At.Before(c.Refills[j].At) })
	if pool := f.pools[provider]; pool != nil && pool.demandKnown {
		unused := round(pool.wasteUnits/total*100, 0)
		c.ProjectedUnusedPercent = &unused
	}
	return c
}
