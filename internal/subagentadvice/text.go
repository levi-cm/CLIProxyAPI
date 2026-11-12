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

// allowanceText summarizes a provider's weekly or monthly allowances: what is
// left and when it expires, when used-up accounts refill, and how much is
// projected to expire unused.
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
	line := fmt.Sprintf("%s %s: %s", p.Provider, p.Allowance[0].Period, strings.Join(parts, "; "))
	if p.ProjectedUnusedPercent != nil && *p.ProjectedUnusedPercent >= 1 {
		line += fmt.Sprintf(" | projected unused at resets: %.0f%% of one account", *p.ProjectedUnusedPercent)
	}
	return line
}

func budgetText(m ModelAdvice, now time.Time) string {
	if m.BudgetTasks == nil {
		if m.Basis == KindConserve {
			return "budget unknown (paid credits)"
		}
		return "budget unknown"
	}
	text := fmt.Sprintf("~%s subagent tasks", roughCount(*m.BudgetTasks))
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
