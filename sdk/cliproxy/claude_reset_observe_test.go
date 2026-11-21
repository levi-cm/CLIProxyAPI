package cliproxy

import (
	"encoding/json"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudereset"
)

// Shape of a live usage response with the reset inventory (values altered).
const claudeResetUsageFixture = `{
 "five_hour": null,
 "seven_day": {"utilization": 90.0, "resets_at": "2026-11-25T14:00:00.229654+00:00"},
 "limits": [
  {"kind": "weekly_all", "percent": 90, "resets_at": "2026-11-25T13:59:59.570899+00:00", "scope": null},
  {"kind": "weekly_scoped", "percent": 100, "resets_at": "2026-11-25T13:59:59.571069+00:00", "scope": {"model": {"id": null, "display_name": "Fable"}}}
 ],
 "cedar_ember": {
  "eligible": true, "ineligible_reason": null, "at_limit": true,
  "exhausted": ["seven_day_overage_included"],
  "grants": [
   {"id": "full-reset-2611", "label": "One full limit reset (November 2026)", "resets_total": 1, "resets_left": 1,
    "starts_at": "2026-11-10T21:30:00+00:00", "ends_at": "2026-12-10T22:00:00+00:00",
    "clears": ["seven_day", "seven_day_overage_included"], "paused": false, "usable_now": true,
    "use_requires_limit": false, "percent_used": {"seven_day": 90}, "blocking": [], "type": "weekly",
    "unavailable_reason": null, "gives_back_weekly": false},
   {"id": "launch-2610-fable", "label": "New model launch: one Fable reset", "resets_total": 1, "resets_left": 1,
    "starts_at": "2026-11-09T18:00:00+00:00", "ends_at": "2027-03-09T07:59:00+00:00",
    "clears": ["seven_day_overage_included"], "paused": false, "usable_now": true,
    "use_requires_limit": null, "blocking": ["x"], "type": "full", "unavailable_reason": null, "gives_back_weekly": true},
   {"id": "session-boost", "label": "Session", "resets_total": 1, "resets_left": 1, "clears": ["five_hour"],
    "usable_now": true, "use_requires_limit": false, "unavailable_reason": "region"}
  ],
  "next_grant_id": "full-reset-2611",
  "weekly_resets_at": "2026-11-25T14:00:00+00:00",
  "cooldown_until": null
 }
}`

func TestClaudeResetObservationFromUsage(t *testing.T) {
	var usage claudeauth.Usage
	if err := json.Unmarshal([]byte(claudeResetUsageFixture), &usage); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	obs := claudeResetObservation(&usage, observed)
	if !obs.Eligible || !obs.AtLimit || !obs.ObservedAt.Equal(observed) || len(obs.Exhausted) != 1 {
		t.Fatalf("program = %+v", obs)
	}
	if !obs.Weekly.Known || obs.Weekly.Used != 90 || obs.Weekly.ResetAt.Format(time.RFC3339) != "2026-11-25T14:00:00Z" || !obs.Weekly.ObservedAt.Equal(observed) {
		t.Fatalf("weekly = %+v", obs.Weekly)
	}
	if !obs.Fable.Known || obs.Fable.Used != 100 || obs.Session.Known {
		t.Fatalf("fable = %+v session = %+v", obs.Fable, obs.Session)
	}
	if len(obs.Grants) != 3 {
		t.Fatalf("grants = %+v", obs.Grants)
	}
	full, fable, session := obs.Grants[0], obs.Grants[1], obs.Grants[2]
	if full.Class != claudereset.ClassWeekly || !full.ClearsWeek || !full.ClearsFable || full.RequiresLimit || full.Blocked || full.EndsAt.Format(time.RFC3339) != "2026-12-10T22:00:00Z" {
		t.Fatalf("full = %+v", full)
	}
	// A missing use_requires_limit is treated as required, as Claude Code does.
	if fable.Class != claudereset.ClassFable || !fable.RequiresLimit || !fable.Blocked {
		t.Fatalf("fable = %+v", fable)
	}
	if session.Class != claudereset.ClassUnsupported || !session.Blocked {
		t.Fatalf("session = %+v", session)
	}
}
