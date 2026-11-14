package subagentadvice

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Text renders the advice as a few short lines for agents with small
// context budgets. It carries everything an orchestrator needs to act and
// never gives a number of agents.
func (a Advice) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s | reuse until %s (%s)\n", postureLine(a.Postures), a.ValidUntil.UTC().Format(time.RFC3339), a.RefreshReason)
	postures := map[string]ProviderPosture{}
	for _, p := range a.Postures {
		postures[p.Provider] = p
		fmt.Fprintf(&b, "%s %s: %s\n", p.Provider, postureName(p.Posture), postureGloss(p, a.models(p.Provider)))
	}
	for _, p := range a.Providers {
		if line := allowanceText(p, a.GeneratedAt); line != "" {
			fmt.Fprintln(&b, line)
		}
		if pp, ok := postures[p.Provider]; ok && pp.Trend != nil {
			fmt.Fprintln(&b, trendText(pp, a.models(p.Provider)))
		}
		if p.OpenCode != nil {
			if line := openCodeText(*p.OpenCode, a.GeneratedAt); line != "" {
				fmt.Fprintln(&b, line)
			}
		}
	}
	for _, m := range a.Models {
		names := make([]string, 0, len(m.Harnesses))
		for name := range m.Harnesses {
			names = append(names, name)
		}
		sort.Strings(names)
		hints := make([]string, 0, len(names))
		for _, name := range names {
			hints = append(hints, name+": "+m.Harnesses[name])
		}
		fmt.Fprintf(&b, "%s effort=%s mode=%s | %s | %s\n", m.Model, m.ReasoningEffort, m.Mode, stepText(m), strings.Join(hints, ", "))
	}
	for _, w := range a.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}

// models returns the advice of a provider's first model, or nil.
func (a Advice) models(provider string) *ModelAdvice {
	for i := range a.Models {
		if a.Models[i].Provider == provider {
			return &a.Models[i]
		}
	}
	return nil
}

// effortName is the human name of an effort; the machine value stays in
// effort= because harnesses take it as is.
func effortName(effort string) string {
	if effort == "xhigh" {
		return "extra high"
	}
	return effort
}

// stepText is the posture and step, for example "use more, extra high + fast".
func stepText(m ModelAdvice) string {
	return postureName(m.Posture) + ", " + effortText(m)
}

// effortText is the effort and mode, for example "extra high + fast".
func effortText(m ModelAdvice) string {
	text := effortName(m.ReasoningEffort)
	if m.Mode != ModeStandard {
		text += " + " + m.Mode
	}
	return text
}

// postureGloss explains a posture in a few words, so the text needs no
// outside explanation. It never gives a number of agents.
func postureGloss(p ProviderPosture, m *ModelAdvice) string {
	switch p.Posture {
	case PostureAvoid:
		return p.Reason
	case PostureConserve:
		return "runs short before reset at the recent pace; use fewer subagents"
	case PostureUseMore:
		if m == nil {
			return "allowance would go unused at the recent pace"
		}
		return "allowance would go unused at the recent pace; " + effortText(*m) + " is fine"
	}
	if m == nil {
		return "use the normal effort"
	}
	return "use " + effortText(*m) + " effort"
}

// trendText compares the pool's daily use over the last days with its even
// daily budget.
func trendText(p ProviderPosture, m *ModelAdvice) string {
	t := *p.Trend
	line := p.Provider + " trend: "
	if t.PercentPerDay == nil {
		line += "unknown (" + t.Unknown + ")"
	} else {
		line += fmt.Sprintf("last %s days %s/day of one account", days(t.CoveredDays), pct(*t.PercentPerDay))
	}
	period := "period"
	if t.PeriodDays > 0 {
		period = days(t.PeriodDays) + " days"
	}
	line += fmt.Sprintf("; even budget %s/day (%d %s x 100%%/%s", pct(t.BudgetPercentPerDay), t.Accounts, plural(t.Accounts, "account"), period)
	if t.CreditsPercent > 0 {
		line += fmt.Sprintf(" + %.0f%% credits over %s days", t.CreditsPercent, days(t.CreditSpendDays))
	}
	line += ") ->"
	if t.Ratio != nil {
		line += fmt.Sprintf(" %.2f of budget:", *t.Ratio)
	}
	if m == nil {
		return line + " " + postureName(p.Posture)
	}
	line += " " + stepText(*m)
	if t.ExpiringSoonPercent != nil && m.Mode != ModeStandard {
		line += fmt.Sprintf(" (%.0f%% of one account expires unused within %s at this trend)", *t.ExpiringSoonPercent, roughDuration(time.Duration(t.ExpiringWithinHours*float64(time.Hour))))
	}
	return line
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// days prints a day count without a needless decimal: 3, 1.5.
func days(v float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

// pct prints a percentage with one decimal below 100 and none above.
func pct(v float64) string {
	if math.Abs(v) >= 100 {
		return fmt.Sprintf("%.0f%%", v)
	}
	return fmt.Sprintf("%.1f%%", v)
}

// openCodeText shows OpenCode Go's windows from the usage tracker.
func openCodeText(u OpenCodeUsage, now time.Time) string {
	if !u.Reachable {
		return ""
	}
	parts := make([]string, 0, len(u.Windows))
	for _, w := range u.Windows {
		part := fmt.Sprintf("%s %.0f%% left", w.Window, w.LeftPercent)
		if w.ResetAt != nil {
			part += ", resets in " + roughDuration(w.ResetAt.Sub(now))
		}
		if !w.Fresh {
			part += " (stale)"
		}
		parts = append(parts, part)
	}
	return "opencode-go quota: " + strings.Join(parts, "; ")
}

// allowanceText summarizes a provider's weekly or monthly allowances: first
// all accounts combined into one, then each account: what is left and when it
// resets, and when used-up accounts refill.
func allowanceText(p ProviderSummary, now time.Time) string {
	if len(p.Allowance) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p.Allowance))
	for _, w := range p.Allowance {
		reset := "reset unknown"
		if w.ResetAt != nil {
			reset = "resets in " + roughDuration(w.ResetAt.Sub(now))
		}
		switch {
		case w.LeftPercent <= 0 && w.ResetAt != nil:
			parts = append(parts, "used up, refills in "+roughDuration(w.ResetAt.Sub(now)))
		case !w.Usable:
			parts = append(parts, fmt.Sprintf("%.0f%% left but unavailable, %s", w.LeftPercent, reset))
		default:
			parts = append(parts, fmt.Sprintf("%.0f%% left, %s", w.LeftPercent, reset))
		}
	}
	line := fmt.Sprintf("%s accounts: %s", p.Provider, strings.Join(parts, "; "))
	if c := p.Combined; c != nil {
		line = combinedText(p.Provider, *c, now) + "\n" + line
	}
	return line
}

// combinedText reads a provider's accounts as one: what is left now, what
// the upcoming resets add back, and what would expire unused.
func combinedText(provider string, c CombinedAllowance, now time.Time) string {
	line := fmt.Sprintf("%s %s, all %d accounts as one: %.0f%% left now", provider, c.Period, c.Accounts, c.LeftPercent)
	if len(c.Refills) > 0 {
		refills := make([]string, 0, len(c.Refills))
		for _, r := range c.Refills {
			refills = append(refills, fmt.Sprintf("+%.0f%% in %s", r.AddPercent, roughDuration(r.At.Sub(now))))
		}
		line += "; refills " + strings.Join(refills, ", ")
	}
	if c.ProjectedUnusedPercent != nil {
		line += fmt.Sprintf("; projected to expire unused: %.0f%%", *c.ProjectedUnusedPercent)
	}
	if cr := c.Credits; cr != nil {
		line += fmt.Sprintf("; credits above the %s floors: %s", roughCredits(cr.FloorPerAccount), roughCredits(cr.Spendable))
		if cr.AddPercent > 0 {
			line += fmt.Sprintf(" (+%.0f%%)", cr.AddPercent)
		}
		switch {
		case !cr.Enabled:
			line += ", unused because credit spending is off in routing"
		case cr.Spendable > 0:
			line += ", spend them"
		}
	}
	if c.ResetGrants > 0 {
		line += fmt.Sprintf("; %d full-reset grants", c.ResetGrants)
		if c.ResetGrantsExpireAt != nil {
			line += " (earliest expires in " + roughDuration(c.ResetGrantsExpireAt.Sub(now)) + ")"
		}
		line += ", redeemed automatically"
	}
	return line
}

// roughCredits prints a credit amount like 7.5k or 106k.
func roughCredits(v float64) string {
	switch {
	case v >= 10000:
		return fmt.Sprintf("%.0fk", v/1000)
	case v >= 1000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v/1000), ".0") + "k"
	}
	return fmt.Sprintf("%.0f", v)
}

func roughDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(math.Max(1, d.Minutes())))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	hours := int(d.Hours())
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dd%dh", hours/24, hours%24)
}
