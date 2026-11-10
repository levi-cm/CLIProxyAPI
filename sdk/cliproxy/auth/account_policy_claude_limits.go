package auth

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Claude accounts can be refused by a window narrower than the weekly one:
// the 5-hour session window blocks every model, and the Fable-scoped weekly
// window ("7d_oi") blocks Fable only. Anthropic reports the Fable window on
// Fable responses only, so the selector keeps the last reported state of each
// window instead of reading it from the latest response.
const (
	claudeFiveHourWindow = "5h"
	claudeFableWindow    = "7d_oi"
)

var claudeLimitWindows = []string{claudeFiveHourWindow, claudeFableWindow}

// claudeLimitResetSlack absorbs sub-second and rounding differences between the
// reset reported by response headers and by the usage endpoint for one window.
const claudeLimitResetSlack = time.Minute

type claudeLimitWindow struct {
	full     bool
	reset    time.Time
	observed time.Time
}

// ObserveResponseHeaders records the limit windows carried by one upstream
// response, including a 429 refusal.
func (s *EarliestDeadlineSelector) ObserveResponseHeaders(authID, provider string, headers http.Header, at time.Time) {
	if len(headers) == 0 || !s.registeredClaudeCredential(provider, authID) {
		return
	}
	s.observeClaudeLimits(authID, headers.Get, at)
}

// registeredClaudeCredential is claudeCredential restricted to accounts the
// manager still holds, so a result that completes after its account was removed
// cannot recreate that account's limit state.
func (s *EarliestDeadlineSelector) registeredClaudeCredential(provider, id string) bool {
	s.mu.Lock()
	resolve := s.bindingAuth
	s.mu.Unlock()
	if resolve == nil {
		return strings.EqualFold(strings.TrimSpace(provider), "claude")
	}
	a, ok := resolve(id)
	return ok && isClaudeAuth(a)
}

// claudeCredential reports whether id is a Claude account. The manager routes
// requests under the multi-provider namespace "mixed", so the provider argument
// alone cannot identify Claude.
func (s *EarliestDeadlineSelector) claudeCredential(provider, id string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return true
	}
	s.mu.Lock()
	resolve := s.bindingAuth
	s.mu.Unlock()
	if resolve == nil || id == "" {
		return false
	}
	a, ok := resolve(id)
	return ok && isClaudeAuth(a)
}

// observeClaudeAuthLimits reads the windows in the auth's stored snapshot, which
// also carries the usage poller's observations of idle accounts.
func (s *EarliestDeadlineSelector) observeClaudeAuthLimits(a *Auth) {
	if !isClaudeAuth(a) || len(a.Quota.Signals) == 0 {
		return
	}
	signals := a.Quota.Signals
	s.observeClaudeLimits(a.ID, func(name string) string { return signals[http.CanonicalHeaderKey(name)] }, a.Quota.ObservedAt)
}

// observeClaudeLimits updates each window the observation reports. A window the
// observation does not mention keeps its previous state.
//
// Results are observed when they complete, but a stream's headers were sent when
// it started, so a long stream can deliver older headers after a newer result.
// Resets only move forward and utilization never falls within one window, so an
// observation of an earlier window, or an open reading of a window already seen
// full, is stale and ignored.
func (s *EarliestDeadlineSelector) observeClaudeLimits(id string, get func(string) string, observed time.Time) {
	if id == "" || observed.IsZero() {
		return
	}
	for _, name := range claudeLimitWindows {
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		status := strings.ToLower(strings.TrimSpace(get(prefix + "Status")))
		rawUsed := strings.TrimSpace(get(prefix + "Utilization"))
		if status == "" && rawUsed == "" {
			continue
		}
		used, errUsed := strconv.ParseFloat(rawUsed, 64)
		next := claudeLimitWindow{
			full:     status == "rejected" || (errUsed == nil && !math.IsNaN(used) && used >= 1),
			observed: observed,
		}
		if seconds, errReset := strconv.ParseInt(strings.TrimSpace(get(prefix+"Reset")), 10, 64); errReset == nil && seconds > 0 {
			next.reset = time.Unix(seconds, 0).UTC()
		}
		s.mu.Lock()
		if s.claudeLimits == nil {
			s.claudeLimits = make(map[string]map[string]claudeLimitWindow)
		}
		windows := s.claudeLimits[id]
		if windows == nil {
			windows = make(map[string]claudeLimitWindow, len(claudeLimitWindows))
			s.claudeLimits[id] = windows
		}
		previous, known := windows[name]
		if known && (!observed.After(previous.observed) || staleClaudeLimit(previous, next)) {
			s.mu.Unlock()
			continue
		}
		windows[name] = next
		s.mu.Unlock()
		if next.full && (!previous.full || !next.reset.Equal(previous.reset)) {
			log.WithFields(log.Fields{"auth": id, "window": name, "reset": next.reset}).Info("account policy: claude limit window full")
		} else if !next.full && previous.full {
			log.WithFields(log.Fields{"auth": id, "window": name}).Info("account policy: claude limit window open")
		}
	}
}

func staleClaudeLimit(previous, next claudeLimitWindow) bool {
	// A reading without a reset cannot prove a newer window, so it never
	// replaces a full window that has not reset yet.
	if next.reset.IsZero() {
		return previous.full && previous.reset.After(next.observed)
	}
	if previous.reset.IsZero() {
		return false
	}
	if next.reset.Before(previous.reset.Add(-claudeLimitResetSlack)) {
		return true
	}
	return previous.full && !next.full && !next.reset.After(previous.reset.Add(claudeLimitResetSlack))
}

// claudeLimitBlocked reports whether a full window that has not reset yet would
// refuse model on the account. A full window without a known reset is left to
// the reactive cooldown, because the selector cannot tell when it reopens.
func (s *EarliestDeadlineSelector) claudeLimitBlocked(id, model string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	windows := s.claudeLimits[id]
	blocked := func(name string) bool {
		w, ok := windows[name]
		return ok && w.full && w.reset.After(now)
	}
	if blocked(claudeFiveHourWindow) {
		return true
	}
	return claudeFableModel(model) && blocked(claudeFableWindow)
}

func claudeFableModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(canonicalModelKey(model)), "claude-fable-")
}
