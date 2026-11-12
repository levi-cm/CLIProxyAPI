package subagentadvice

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

var postureText = map[string]string{
	PostureSpendFreely: "spend freely: allowance would expire unused, so many parallel subagents and fast mode are welcome",
	PostureNormal:      "normal: use as many subagents as the work needs",
	PostureConserve:    "conserve: quota is short before reset; few subagents, low effort",
	PostureUnavailable: "unavailable: start no new subagents",
}

// Text renders the advice as a few short lines for agents with small
// context budgets. It carries everything an orchestrator needs to act.
func (a Advice) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s | reuse until %s (%s)\n", postureText[a.Posture], a.ValidUntil.UTC().Format(time.RFC3339), a.RefreshReason)
	for _, p := range a.Providers {
		if line := allowanceText(p, a.GeneratedAt); line != "" {
			fmt.Fprintln(&b, line)
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
		fmt.Fprintf(&b, "%s effort=%s mode=%s | %s | %s\n", m.Model, m.ReasoningEffort, m.Mode, budgetText(m, a.GeneratedAt), strings.Join(hints, ", "))
	}
	for _, w := range a.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
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

func budgetText(m ModelAdvice, now time.Time) string {
	if m.BudgetTasks == nil {
		if m.Basis == KindConserve {
			return "budget unknown (paid credits)"
		}
		return "budget unknown"
	}
	text := fmt.Sprintf("~%s subagent tasks", roughCount(*m.BudgetTasks))
	if m.IncludesCredits {
		if m.BudgetUntil != nil {
			return text + " of expiring allowance (use within " + roughDuration(m.BudgetUntil.Sub(now)) + ") and credits above the floors"
		}
		return text + " of credits above the floors"
	}
	if m.BudgetUntil != nil {
		in := roughDuration(m.BudgetUntil.Sub(now))
		switch m.Basis {
		case KindSurplus:
			text += " of allowance that expires unused; use within " + in
		case KindConserve:
			text += " until the next refill in " + in
		default:
			text += " before the allowance resets in " + in
		}
	}
	return text
}

// roughCount keeps two significant digits so budgets read as estimates.
func roughCount(n int) string {
	if n < 100 {
		return strconv.Itoa(n)
	}
	scale := int(math.Pow(10, math.Floor(math.Log10(float64(n)))-1))
	return strconv.Itoa(n / scale * scale)
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
