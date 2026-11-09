package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Claude subscription accounts report their weekly window on every response.
// Deadline routing uses these passive observations; it never polls Anthropic.
const (
	claudeWeeklyResetSignal       = "Anthropic-Ratelimit-Unified-7d-Reset"
	claudeWeeklyUtilizationSignal = "Anthropic-Ratelimit-Unified-7d-Utilization"
	claudeWeek                    = 7 * 24 * time.Hour

	// Focus mode only considers the account whose weekly allowance resets within
	// this horizon.
	claudeFocusHorizon = 48 * time.Hour
	// Focus mode starts when at least this many percentage points of that account
	// are projected to expire unused, and stops once no leftover is projected.
	claudeFocusMinLeftover = 10.0
	// The projection uses the account's own usage pace over this trailing window,
	// and only after at least claudePaceMinSpan of observations.
	claudePaceWindow     = 24 * time.Hour
	claudePaceMinSpan    = time.Hour
	claudeSampleInterval = 5 * time.Minute
)

type claudeUsageSample struct {
	at   time.Time
	used float64
}

// claudeWeeklyState is the last observed weekly window of one Claude account.
type claudeWeeklyState struct {
	reset    time.Time
	used     float64
	observed time.Time
	samples  []claudeUsageSample
}

func isClaudeAuth(a *Auth) bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), "claude")
}

// observeClaude records the auth's latest passive weekly-window observation.
func (s *EarliestDeadlineSelector) observeClaude(a *Auth) {
	if !isClaudeAuth(a) || a.Quota.ObservedAt.IsZero() {
		return
	}
	seconds, errReset := strconv.ParseInt(strings.TrimSpace(a.Quota.Signals[claudeWeeklyResetSignal]), 10, 64)
	if errReset != nil || seconds <= 0 {
		return
	}
	used, errUsed := strconv.ParseFloat(strings.TrimSpace(a.Quota.Signals[claudeWeeklyUtilizationSignal]), 64)
	if errUsed != nil || math.IsNaN(used) || math.IsInf(used, 0) {
		return
	}
	used = math.Min(math.Max(used, 0), 1) * 100
	reset := time.Unix(seconds, 0).UTC()
	observed := a.Quota.ObservedAt

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claude == nil {
		s.claude = make(map[string]*claudeWeeklyState)
	}
	state := s.claude[a.ID]
	if state == nil {
		state = &claudeWeeklyState{}
		s.claude[a.ID] = state
	}
	if !observed.After(state.observed) {
		return
	}
	// A new weekly window or a provider correction invalidates the pace history.
	if !reset.Equal(state.reset) || used < state.used {
		state.samples = nil
	}
	state.reset, state.used, state.observed = reset, used, observed
	if n := len(state.samples); n == 0 || observed.Sub(state.samples[n-1].at) >= claudeSampleInterval {
		state.samples = append(state.samples, claudeUsageSample{at: observed, used: used})
	}
	cutoff := observed.Add(-claudePaceWindow)
	for len(state.samples) > 1 && state.samples[0].at.Before(cutoff) {
		state.samples = state.samples[1:]
	}
}

// claudeDeadline returns the account's next weekly reset. A passed reset means a
// fresh window began; it is projected forward by whole weeks for ordering only.
func (s *EarliestDeadlineSelector) claudeDeadline(a *Auth, now time.Time) (time.Time, bool) {
	s.observeClaude(a)
	s.mu.Lock()
	state := s.claude[a.ID]
	var reset time.Time
	if state != nil {
		reset = state.reset
	}
	s.mu.Unlock()
	if reset.IsZero() {
		return time.Time{}, false
	}
	for !reset.After(now) {
		reset = reset.Add(claudeWeek)
	}
	return reset, true
}

// pace returns percentage points per hour over the trailing window, measured up
// to now so idle time after the last response lowers the pace.
func (st *claudeWeeklyState) pace(now time.Time) (float64, bool) {
	start := now.Add(-claudePaceWindow)
	for _, sample := range st.samples {
		if sample.at.Before(start) {
			continue
		}
		span := now.Sub(sample.at)
		if span < claudePaceMinSpan {
			return 0, false
		}
		return math.Max(0, st.used-sample.used) / span.Hours(), true
	}
	return 0, false
}

// claudeFocusTarget returns the account that should absorb running Claude
// conversations: the earliest weekly reset within claudeFocusHorizon that, at its
// recent pace, would otherwise expire with unused allowance. It returns "" when
// no account would lose allowance, so running conversations keep their account.
func (s *EarliestDeadlineSelector) claudeFocusTarget(now time.Time) string {
	disabled := strings.EqualFold(strings.TrimSpace(s.policy.Settings().ClaudeFocus), "off")
	s.mu.Lock()
	defer s.mu.Unlock()
	targetID := ""
	var target *claudeWeeklyState
	for id, state := range s.claude {
		if !state.reset.After(now) {
			continue
		}
		if target == nil || state.reset.Before(target.reset) || (state.reset.Equal(target.reset) && id < targetID) {
			targetID, target = id, state
		}
	}
	active := ""
	leftover, pace := 0.0, 0.0
	if !disabled && target != nil && target.reset.Sub(now) <= claudeFocusHorizon {
		if measured, ok := target.pace(now); ok {
			pace = measured
			leftover = (100 - target.used) - pace*target.reset.Sub(now).Hours()
			threshold := claudeFocusMinLeftover
			if s.claudeFocus == targetID {
				threshold = 0
			}
			if leftover > 0 && leftover >= threshold {
				active = targetID
			}
		}
	}
	if active != s.claudeFocus {
		fields := log.Fields{"previous": s.claudeFocus, "target": active, "pace_points_per_hour": math.Round(pace*100) / 100, "projected_leftover_points": math.Round(leftover*10) / 10}
		if target != nil {
			fields["reset"] = target.reset
			fields["used_percent"] = target.used
		}
		log.WithFields(fields).Info("account policy: claude focus changed")
		s.claudeFocus = active
	}
	return active
}

// claudeConversationPortable reports whether a Claude turn can continue on
// another account at the cost of a prompt-cache rebuild only. Sonnet 5.5 and
// Haiku 5.5 thinking is bound to the producing account, so only model families
// whose thinking stays valid are moved. Workspace-scoped server state (files,
// containers, server tool results) keeps the conversation on its account.
func claudeConversationPortable(ctx context.Context, model string, opts cliproxyexecutor.Options) bool {
	name := strings.ToLower(canonicalModelKey(model))
	portable := false
	for _, family := range []string{"claude-opus-", "claude-fable-", "claude-mythos-"} {
		if strings.HasPrefix(name, family) {
			portable = true
			break
		}
	}
	if !portable || !policyRequestReplayable(ctx, opts) {
		return false
	}
	return !claudeRequestAccountBound(opts.OriginalRequest)
}

func claudeRequestAccountBound(body []byte) bool {
	if gjson.GetBytes(body, "container").Exists() {
		return true
	}
	fileSource := func(block gjson.Result) bool {
		return block.Get("source.type").String() == "file" || block.Get("source.file_id").Exists()
	}
	bound := false
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			kind := block.Get("type").String()
			if strings.HasSuffix(kind, "_tool_result") || kind == "container_upload" || fileSource(block) {
				bound = true
				return false
			}
			block.Get("content").ForEach(func(_, nested gjson.Result) bool {
				if fileSource(nested) {
					bound = true
				}
				return !bound
			})
			return !bound
		})
		return !bound
	})
	return bound
}
