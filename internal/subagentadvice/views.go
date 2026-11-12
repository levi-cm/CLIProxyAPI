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
		if st.paceKnown && !reset.IsZero() {
			leftover := round(math.Max(0, 100-st.used-st.pace*reset.Sub(f.now).Hours()), 2)
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
		p.UsableSurplusUnits += st.surplusUnits
	}
	sort.Strings(names)
	out := make([]ProviderSummary, 0, len(names))
	for _, name := range names {
		p := byProvider[name]
		p.UsableSurplusUnits = round(p.UsableSurplusUnits, 2)
		if pool := f.pools[name]; pool != nil {
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
				p.State, p.Note = "tight", "forecast demand exceeds capacity before the next reset"
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
