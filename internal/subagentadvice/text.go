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

func budgetText(m ModelAdvice, now time.Time) string {
	if m.BudgetTasks == nil {
		return "budget unknown"
	}
	text := fmt.Sprintf("~%s subagent tasks", roughCount(*m.BudgetTasks))
	if m.BudgetUntil != nil {
		text += fmt.Sprintf(" before reset in %s", roughDuration(m.BudgetUntil.Sub(now)))
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
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
