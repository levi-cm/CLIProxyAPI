package auth

import (
	"context"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// claudeUsagePollInterval keeps an idle account's observation well inside
	// the 15-minute freshness window used by dashboards and routing evidence.
	claudeUsagePollInterval = 5 * time.Minute
	// claudeUsageIdleAge skips accounts whose inference traffic already
	// refreshed the observation recently.
	claudeUsageIdleAge = 4 * time.Minute
	// claudeUsageFetchTimeout bounds one control-plane read; it never applies to
	// inference traffic.
	claudeUsageFetchTimeout = 30 * time.Second
)

// ClaudeUsageFetcher reads an account's plan utilization without a model
// request and returns it as unified rate-limit headers.
type ClaudeUsageFetcher func(ctx context.Context, auth *Auth, accessToken string) (http.Header, error)

// StartClaudeUsagePoller keeps the quota observation of Claude accounts fresh
// while they serve no traffic, so usage views and deadline routing do not go
// stale for accounts routing is deliberately keeping idle.
func (m *Manager) StartClaudeUsagePoller(ctx context.Context, fetch ClaudeUsageFetcher) {
	if m == nil || fetch == nil {
		return
	}
	go func() {
		m.pollClaudeUsage(ctx, fetch)
		ticker := time.NewTicker(claudeUsagePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.pollClaudeUsage(ctx, fetch)
			}
		}
	}()
}

func (m *Manager) pollClaudeUsage(ctx context.Context, fetch ClaudeUsageFetcher) {
	for _, a := range m.claudeUsagePollCandidates(time.Now()) {
		if ctx.Err() != nil {
			return
		}
		token := authAccessToken(a)
		started := time.Now()
		fetchCtx, cancel := context.WithTimeout(ctx, claudeUsageFetchTimeout)
		headers, errFetch := fetch(fetchCtx, a, token)
		cancel()
		if errFetch != nil {
			log.Debugf("claude usage poll skipped for %s: %v", a.ID, errFetch)
			continue
		}
		m.observeClaudeUsage(a.ID, a.RegistrationEpoch, headers, started)
	}
}

// claudeUsagePollCandidates returns enabled Claude accounts with a usable
// token whose last observation is older than claudeUsageIdleAge.
func (m *Manager) claudeUsagePollCandidates(now time.Time) []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Auth
	for _, a := range m.auths {
		if !isClaudeAuth(a) || a.Disabled || a.Status == StatusDisabled || hasUnauthorizedAuthFailure(a) {
			continue
		}
		if authAccessToken(a) == "" {
			continue
		}
		if exp, ok := a.AccessTokenExpirationTime(); ok && !exp.IsZero() && !exp.After(now) {
			continue
		}
		if !a.Quota.ObservedAt.IsZero() && now.Sub(a.Quota.ObservedAt) < claudeUsageIdleAge {
			continue
		}
		out = append(out, a.Clone())
	}
	return out
}

// observeClaudeUsage stores a polled observation unless the auth was
// re-registered or a real response observed it after the poll started. Only
// the passive observation changes; cooldown and scheduling fields are untouched.
func (m *Manager) observeClaudeUsage(id string, epoch uint64, headers http.Header, started time.Time) bool {
	releaseMutation := m.lockAuthMutation(id)
	defer releaseMutation()
	m.mu.Lock()
	a, ok := m.auths[id]
	if !ok || a == nil || a.RegistrationEpoch != epoch || a.Quota.ObservedAt.After(started) {
		m.mu.Unlock()
		return false
	}
	now := time.Now()
	if !a.Quota.ObserveResponseHeadersForProvider(a.Provider, headers, now) {
		m.mu.Unlock()
		return false
	}
	a.Generation++
	a.UpdatedAt = now
	snapshot := a.Clone()
	m.mu.Unlock()
	releaseMutation()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	// A later response replaces the snapshot, so hand the windows to the
	// selector now; the Fable window is otherwise visible on Fable responses only.
	m.observeSelectorResponseHeaders(Result{AuthID: id, Provider: snapshot.Provider, RegistrationEpoch: epoch}, headers, now)
	return true
}
