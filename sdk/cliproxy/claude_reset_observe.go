package cliproxy

import (
	"math"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudereset"
)

// claudeResetObservation converts a usage response that includes the reset inventory.
func claudeResetObservation(u *claudeauth.Usage, observed time.Time) claudereset.Observation {
	obs := claudereset.Observation{ObservedAt: observed.UTC()}
	if u == nil {
		return obs
	}
	obs.Weekly = claudeResetWindow(u.WeeklyWindow(), obs.ObservedAt)
	obs.Fable = claudeResetWindow(u.FableWindow(), obs.ObservedAt)
	obs.Session = claudeResetWindow(u.SessionWindow(), obs.ObservedAt)
	program := u.Resets
	if program == nil {
		return obs
	}
	obs.Eligible = program.Eligible
	if program.IneligibleReason != nil {
		obs.IneligibleReason = strings.TrimSpace(*program.IneligibleReason)
	}
	obs.AtLimit = program.AtLimit
	obs.CooldownUntil = claudeResetTime(program.CooldownUntil)
	obs.Exhausted = append([]string(nil), program.Exhausted...)
	if obs.Weekly.ResetAt.IsZero() {
		obs.Weekly.ResetAt = claudeResetTime(program.WeeklyResetsAt)
	}
	// The Fable window renews with the weekly window.
	if obs.Fable.ResetAt.IsZero() {
		obs.Fable.ResetAt = obs.Weekly.ResetAt
	}
	for _, g := range program.Grants {
		requiresLimit := true
		if g.UseRequiresLimit != nil {
			requiresLimit = *g.UseRequiresLimit
		}
		unavailable := g.UnavailableReason != nil && strings.TrimSpace(*g.UnavailableReason) != ""
		class, clearsWeekly, clearsFable := claudereset.ClassOf(g.Clears)
		obs.Grants = append(obs.Grants, claudereset.Grant{
			ID:            g.ID,
			Label:         strings.TrimSpace(g.Label),
			Class:         class,
			ClearsWeek:    clearsWeekly,
			ClearsFable:   clearsFable,
			ResetsTotal:   g.ResetsTotal,
			ResetsLeft:    g.ResetsLeft,
			StartsAt:      claudeResetTime(g.StartsAt),
			EndsAt:        claudeResetTime(g.EndsAt),
			Paused:        g.Paused,
			UsableNow:     g.UsableNow,
			RequiresLimit: requiresLimit,
			Blocked:       len(g.Blocking) > 0 || unavailable,
		})
	}
	return obs
}

func claudeResetWindow(w *claudeauth.UsageWindow, observed time.Time) claudereset.Window {
	if w == nil || w.Utilization == nil {
		return claudereset.Window{}
	}
	used := *w.Utilization
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return claudereset.Window{}
	}
	return claudereset.Window{Known: true, Used: used, ResetAt: claudeResetTime(&w.ResetsAt), ObservedAt: observed}
}

func claudeResetTime(raw *string) time.Time {
	if raw == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*raw))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
