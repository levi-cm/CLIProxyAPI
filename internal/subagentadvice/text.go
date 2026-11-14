package subagentadvice

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/fleetactivity"
)

// Text renders the advice as a few short lines for agents with small
// context budgets. It carries everything an orchestrator needs to act.
func (a Advice) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s | reuse until %s (%s)\n", postureLine(a), a.ValidUntil.UTC().Format(time.RFC3339), a.RefreshReason)
	for _, p := range a.Providers {
		if line := allowanceText(p, a.GeneratedAt); line != "" {
			fmt.Fprintln(&b, line)
		}
		if p.Fleet != nil {
			fmt.Fprintln(&b, fleetText(p.Provider, *p.Fleet))
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
		fmt.Fprintf(&b, "%s effort=%s mode=%s | %s | %s | %s\n", m.Model, m.ReasoningEffort, m.Mode, stepText(m), roomText(m), strings.Join(hints, ", "))
	}
	if line := agentsText(a.Fleet); line != "" {
		fmt.Fprintln(&b, line)
	}
	for _, w := range a.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}

// postureLine is the first line: what every orchestrator should do now.
func postureLine(a Advice) string {
	working := a.Fleet.Agents
	switch a.Posture {
	case PostureRoom:
		return fmt.Sprintf("room for ~%d more %s across all orchestrators (%d working now); start at most that many", a.RoomAgents, plural(a.RoomAgents, "agent"), working)
	case PostureHold:
		return fmt.Sprintf("hold: %d %s working already use the target rate; start no new agents", working, plural(working, "agent"))
	case PostureConserve:
		return fmt.Sprintf("conserve: quota runs out before it resets at this rate; start no new agents, lowest effort (%d working now)", working)
	case PostureUnknown:
		return fmt.Sprintf("budget unknown: no quota telemetry; start few agents (%d working now)", working)
	}
	return "unavailable: start no new subagents"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// effortName is the human name of an effort; the machine value stays in
// effort= because harnesses take it as is.
func effortName(effort string) string {
	if effort == "xhigh" {
		return "extra high"
	}
	return effort
}

func stepText(m ModelAdvice) string {
	text := effortName(m.ReasoningEffort)
	if m.Mode != ModeStandard {
		text += " + " + m.Mode
	}
	if m.Steps > 1 {
		text += fmt.Sprintf(", step %d of %d", m.Step, m.Steps)
	}
	return text
}

func roomText(m ModelAdvice) string {
	switch {
	case m.Basis == KindHold:
		return "hold: no new agents"
	case m.Basis == KindConserve:
		return "conserve: no new agents"
	case m.RoomAgents == nil:
		if strings.Contains(m.Reason, "usage tracker") {
			return "room unknown (quota per tracker above)"
		}
		return "budget unknown"
	}
	return fmt.Sprintf("room for ~%d more", *m.RoomAgents)
}

// fleetText compares the pool's recent use with its target rate.
func fleetText(provider string, r FleetRate) string {
	line := fmt.Sprintf("%s fleet: %d %s working, recent %s/h", provider, r.Agents, plural(r.Agents, "agent"), rate(r.RecentPercentPerHour))
	if r.RecentBasis == "estimated" {
		line += " (estimated)"
	}
	line += fmt.Sprintf(" of one account; target %s/h (%.0f%% of the %s/h that uses every account by its reset", rate(r.TargetPercentPerHour), fraction(r), rate(r.NeedPercentPerHour))
	if r.CreditsPercentPerHour > 0 {
		line += fmt.Sprintf(", incl. %s/h of credits", rate(r.CreditsPercentPerHour))
	}
	line += fmt.Sprintf("); ~%s/h per agent", rate(r.PerAgentPercentPerHour))
	switch r.PerAgentBasis {
	case PerAgentTaskEstimate:
		line += " (estimated from task size)"
	case PerAgentLastMeasured:
		line += " (last measured)"
	}
	switch r.State {
	case RateRoom:
		line += fmt.Sprintf(" -> room for ~%d more", r.RoomAgents)
	case RateHold:
		line += " -> hold"
	default:
		line += " -> conserve"
	}
	if r.TimeSensitive {
		line += fmt.Sprintf("; %.0f%% of one account expires unused within a day even at the target", r.ExpiringSoonPercent)
	}
	return line
}

func fraction(r FleetRate) float64 {
	if r.NeedPercentPerHour <= 0 {
		return 0
	}
	return r.TargetPercentPerHour / r.NeedPercentPerHour * 100
}

func rate(v float64) string {
	if v != 0 && math.Abs(v) < 0.1 {
		return fmt.Sprintf("%.2f%%", v)
	}
	return fmt.Sprintf("%.1f%%", v)
}

// openCodeText shows OpenCode Go's windows, or when to come back.
func openCodeText(u OpenCodeUsage, now time.Time) string {
	if !u.Reachable {
		return ""
	}
	if u.AvoidUntil != nil {
		return "OpenCode Go: avoid until " + u.AvoidUntil.UTC().Format("Jan 2 15:04 MST") + " (" + u.AvoidWhy + ")"
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
	return "OpenCode Go: " + strings.Join(parts, "; ")
}

// agentsText lists who is working, by provider and client device.
func agentsText(s fleetactivity.Snapshot) string {
	if s.Agents == 0 {
		return fmt.Sprintf("agents working (last %s): none", roughDuration(time.Duration(s.WindowSeconds)*time.Second))
	}
	var providers, devices []string
	for _, name := range fleetactivity.SortedNames(s.Providers) {
		providers = append(providers, fmt.Sprintf("%s %d", name, s.Providers[name].Agents))
	}
	for _, name := range fleetactivity.SortedNames(s.Devices) {
		devices = append(devices, fmt.Sprintf("%s %d", name, s.Devices[name].Agents))
	}
	return fmt.Sprintf("agents working (last %s): %d (%d requests in flight); %s; devices %s", roughDuration(time.Duration(s.WindowSeconds)*time.Second), s.Agents, s.InFlight, strings.Join(providers, ", "), strings.Join(devices, ", "))
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
