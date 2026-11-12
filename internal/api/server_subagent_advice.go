package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyledger"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/subagentadvice"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	subagentAdvicePath       = "/v1/account-policy/subagent-advice"
	subagentForecastPath     = "/v1/account-policy/subagent-forecast"
	subagentSampleInterval   = 5 * time.Minute
	subagentLedgerTTL        = 5 * time.Minute
	claudeLongWindowSeconds  = 7 * 24 * 3600
	claudeShortWindowSeconds = 5 * 3600
)

// subagentInventory reads the live account state for the subagent advisor.
// It only reads; it never changes routing, credentials or policy state.
type subagentInventory struct {
	manager *auth.Manager
	policy  *accountpolicy.Service
	history func() *accountpolicyledger.Archive
	now     func() time.Time

	mu      sync.Mutex
	usageAt time.Time
	usage   map[string]*subagentadvice.UsageHistory
	allowAt map[string]time.Time
	allow   map[string][]subagentadvice.Sample
}

func newSubagentInventory(manager *auth.Manager, policy *accountpolicy.Service, history func() *accountpolicyledger.Archive) *subagentInventory {
	return &subagentInventory{manager: manager, policy: policy, history: history, now: time.Now, allowAt: map[string]time.Time{}, allow: map[string][]subagentadvice.Sample{}}
}

type claudeLimitSource interface {
	ClaudeLimitWindows(id string, epoch uint64) map[string]auth.ClaudeLimitWindowState
}

// Inventory implements subagentadvice.InventorySource.
func (r *subagentInventory) Inventory(ctx context.Context) (subagentadvice.Inventory, error) {
	if r == nil || r.manager == nil {
		return subagentadvice.Inventory{}, errors.New("auth manager unavailable")
	}
	now := r.now().UTC()
	auths := r.manager.List()
	ids := make([]string, 0, len(auths))
	for _, a := range auths {
		if a != nil {
			ids = append(ids, a.ID)
		}
	}
	runtime, _ := r.manager.PolicyRuntimeSnapshot(ids)
	var settings accountpolicy.Settings
	if r.policy != nil {
		settings = r.policy.Settings()
	}
	limits, _ := r.manager.Selector().(claudeLimitSource)
	usage := r.ledgerUsage(ctx, now)
	reg := registry.GetGlobalRegistry()

	inv := subagentadvice.Inventory{RoutingOverrides: map[string]string{}, RoutingAccounts: map[string]string{}}
	if r.policy != nil && settings.Enabled {
		if o := r.policy.RoutingOverride(); o.State == "active" && o.Provider != "" {
			mode := o.Mode
			if mode == "" {
				mode = "single"
			}
			provider := strings.ToLower(o.Provider)
			inv.RoutingOverrides[provider] = mode
			if mode == "single" && o.CredentialID != "" {
				inv.RoutingAccounts[provider] = o.CredentialID
			}
		}
	}
	type providerModel struct{ provider, id string }
	modelSet := map[providerModel]bool{}
	for _, a := range auths {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(a.Provider))
		acct := subagentadvice.Account{ID: a.ID, Provider: provider, ActiveRequests: runtime[a.ID].ActiveRequests, ModelBlocks: map[string]subagentadvice.Block{}}
		for _, m := range reg.GetModelsForClient(a.ID) {
			if m == nil || strings.TrimSpace(m.ID) == "" {
				continue
			}
			acct.Models = append(acct.Models, m.ID)
			if canonical := strings.TrimSpace(m.MetadataModelID); canonical != "" && canonical != m.ID {
				if acct.Canonical == nil {
					acct.Canonical = map[string]string{}
				}
				acct.Canonical[m.ID] = canonical
			}
			modelSet[providerModel{provider, m.ID}] = true
		}
		sort.Strings(acct.Models)
		r.blockState(a, &acct, now)
		acct.ReservePercent = settings.Accounts[a.ID].ReservePercent
		switch provider {
		case "claude":
			acct.Windows = claudeWindows(a, limits)
		default:
			r.policyWindows(a, &acct)
		}
		r.policyEligibility(a, &acct, settings, now)
		if u, ok := usage[a.ID]; ok {
			acct.Usage = u
		} else if usage != nil {
			acct.Usage = &subagentadvice.UsageHistory{}
		}
		if len(acct.Windows) > 0 && provider != "claude" {
			acct.History = r.allowanceHistory(ctx, a.ID, now)
		}
		inv.Accounts = append(inv.Accounts, acct)
	}
	// Metadata is kept per provider: two providers may register the same ID
	// with different context limits or reasoning levels.
	pairs := make([]providerModel, 0, len(modelSet))
	for pm := range modelSet {
		pairs = append(pairs, pm)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].provider != pairs[j].provider {
			return pairs[i].provider < pairs[j].provider
		}
		return pairs[i].id < pairs[j].id
	})
	for _, pm := range pairs {
		info := reg.GetModelInfo(pm.id, pm.provider)
		dm := subagentadvice.DiscoveredModel{ID: pm.id, Provider: pm.provider}
		if info != nil {
			if info.Thinking != nil {
				dm.Levels = append([]string(nil), info.Thinking.Levels...)
			}
			dm.ContextTokens = info.ContextLength
			if dm.ContextTokens == 0 {
				dm.ContextTokens = info.InputTokenLimit
			}
			dm.MaxOutputTokens = info.MaxCompletionTokens
		}
		inv.Models = append(inv.Models, dm)
	}
	return inv, nil
}

// blockState maps lifecycle, cooldown and per-model state to blocks.
func (r *subagentInventory) blockState(a *auth.Auth, acct *subagentadvice.Account, now time.Time) {
	switch {
	case a.Disabled || a.Status == auth.StatusDisabled:
		acct.Blocked = "disabled"
	case a.Status == auth.StatusError && a.LastError != nil && (a.LastError.HTTPStatus == http.StatusUnauthorized || a.LastError.HTTPStatus == http.StatusForbidden):
		acct.Blocked = "auth_error"
	case a.Status == auth.StatusPending:
		acct.Blocked = "auth_pending"
	case a.Quota.Exceeded && a.Quota.NextRecoverAt.After(now):
		acct.Blocked, acct.BlockedUntil = "rate_limited", a.Quota.NextRecoverAt
	case a.Unavailable && a.NextRetryAfter.After(now):
		acct.Blocked, acct.BlockedUntil = "cooldown", a.NextRetryAfter
	}
	for model, state := range a.ModelStates {
		if state == nil || !state.Unavailable {
			continue
		}
		until := state.NextRetryAfter
		if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(until) {
			until = state.Quota.NextRecoverAt
		}
		if !until.After(now) {
			continue
		}
		block := subagentadvice.Block{Reason: "model_cooldown", Until: until}
		acct.ModelBlocks[model] = block
		if a.Prefix != "" && !strings.HasPrefix(model, a.Prefix+"/") {
			acct.ModelBlocks[a.Prefix+"/"+model] = block
		}
	}
}

// claudeWindows reads the passive unified rate-limit headers, which the usage
// poller also refreshes for idle accounts, and the selector's remembered
// limit windows (the Fable window is reported on Fable responses only).
func claudeWindows(a *auth.Auth, limits claudeLimitSource) []subagentadvice.Window {
	signals := a.Quota.Signals
	observed := a.Quota.ObservedAt
	get := func(name string) string { return strings.TrimSpace(signals[http.CanonicalHeaderKey(name)]) }
	var out []subagentadvice.Window
	read := func(name string, long bool, model string, duration int64) {
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		rawUsed, status := get(prefix+"Utilization"), strings.ToLower(get(prefix+"Status"))
		if observed.IsZero() || (rawUsed == "" && status == "") {
			return
		}
		w := subagentadvice.Window{Name: name, Long: long, Model: model, DurationSeconds: duration, ObservedAt: observed, Full: status == "rejected"}
		if used, err := strconv.ParseFloat(rawUsed, 64); err == nil {
			w.UsedPercent = used * 100
		} else if !w.Full {
			return
		}
		if seconds, err := strconv.ParseInt(get(prefix+"Reset"), 10, 64); err == nil && seconds > 0 {
			w.ResetAt = time.Unix(seconds, 0).UTC()
		}
		out = append(out, w)
	}
	read("7d", true, "", claudeLongWindowSeconds)
	read("5h", false, "", claudeShortWindowSeconds)
	read("7d_oi", true, "claude-fable-*", claudeLongWindowSeconds)
	if limits == nil {
		return out
	}
	for name, state := range limits.ClaudeLimitWindows(a.ID, a.RegistrationEpoch) {
		model, long := "", false
		if name == "7d_oi" {
			model, long = "claude-fable-*", true
		}
		replaced := false
		for i := range out {
			if out[i].Name == name {
				replaced = true
				// The selector keeps a window full until its reset. A late
				// header from a stream that began earlier carries a newer
				// timestamp but older utilization, so only a header for a
				// later window (a later reset) may reopen it.
				laterWindow := !state.Reset.IsZero() && out[i].ResetAt.After(state.Reset.Add(2*time.Minute))
				if state.Full && !laterWindow {
					out[i].Full, out[i].UsedPercent = true, 100
					if !state.Reset.IsZero() {
						out[i].ResetAt = state.Reset
					}
					if state.Observed.After(out[i].ObservedAt) {
						out[i].ObservedAt = state.Observed
					}
				}
			}
		}
		if !replaced && state.Full {
			out = append(out, subagentadvice.Window{Name: name, Long: long, Model: model, ObservedAt: state.Observed, ResetAt: state.Reset, Full: true, UsedPercent: 100})
		}
	}
	return out
}

// policyWindows maps account-policy snapshots (Codex today) to windows.
func (r *subagentInventory) policyWindows(a *auth.Auth, acct *subagentadvice.Account) {
	if r.policy == nil {
		return
	}
	snapshot, ok := r.policy.Snapshot(a.ID)
	if !ok || snapshot.Identity.CredentialID != a.ID {
		return
	}
	acct.Plan = snapshot.Plan
	for _, b := range snapshot.Buckets {
		observed := b.ObservedAt
		if observed.IsZero() {
			observed = snapshot.ObservedAt
		}
		name := b.Scope
		switch {
		case b.DurationSeconds >= 6*24*3600:
			name = "7d"
		case b.DurationSeconds == claudeShortWindowSeconds:
			name = "5h"
		case name == "":
			name = strconv.FormatInt(b.DurationSeconds, 10) + "s"
		}
		acct.Windows = append(acct.Windows, subagentadvice.Window{
			Name:            name,
			Long:            b.DurationSeconds >= 6*24*3600,
			Model:           strings.TrimSpace(b.Model),
			UsedPercent:     b.UsedPercent,
			ResetAt:         b.ResetAt,
			DurationSeconds: b.DurationSeconds,
			ObservedAt:      observed,
			// The policy treats a disallowed bucket as blocked, whatever its percentage.
			Full: b.UsedPercent >= 100 || (b.Allowed != nil && !*b.Allowed),
		})
	}
}

// policyEligibility applies the verdict the account-policy selector uses for
// every provider (holds, forced account, reserves, exhaustion), so plans never
// pin a slot to an account the router would refuse.
func (r *subagentInventory) policyEligibility(a *auth.Auth, acct *subagentadvice.Account, settings accountpolicy.Settings, now time.Time) {
	if r.policy == nil || !settings.Enabled || acct.Blocked != "" {
		return
	}
	snapshot, ok := r.policy.Snapshot(a.ID)
	if !ok {
		snapshot.Identity = accountpolicy.Identity{CredentialID: a.ID, Provider: a.Provider}
	}
	if snapshot.Identity.CredentialID != a.ID || snapshot.Identity.Provider != a.Provider {
		return
	}
	evaluation := accountpolicy.Evaluate(snapshot, "", settings, now)
	acct.ServingOnCredits = evaluation.Credits
	if evaluation.Eligible {
		return
	}
	if evaluation.Reason == "quota_block" {
		acct.Blocked = "allowance_exhausted"
		for _, w := range acct.Windows {
			if w.Full && w.ResetAt.After(now) && (acct.BlockedUntil.IsZero() || w.ResetAt.Before(acct.BlockedUntil)) {
				acct.BlockedUntil = w.ResetAt
			}
		}
		return
	}
	acct.Blocked = "policy_" + evaluation.Reason
}

func (r *subagentInventory) archive() *accountpolicyledger.Archive {
	if r.history == nil {
		return nil
	}
	return r.history()
}

// ledgerUsage returns token totals per credential for the last 24 and 3 hours,
// or nil when the history archive is unavailable. Results are cached briefly.
func (r *subagentInventory) ledgerUsage(ctx context.Context, now time.Time) map[string]*subagentadvice.UsageHistory {
	a := r.archive()
	if a == nil || !a.Status().Healthy {
		return nil
	}
	r.mu.Lock()
	if r.usage != nil && now.Sub(r.usageAt) < subagentLedgerTTL {
		cached := r.usage
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()
	collect := func(from time.Time) (map[string][]subagentadvice.ModelUsage, error) {
		totals, err := a.SummaryContext(ctx, accountpolicyledger.Query{From: from, To: now, Limit: 100}, "day")
		if err != nil {
			return nil, err
		}
		merged := map[string]map[string]*subagentadvice.ModelUsage{}
		for _, t := range totals {
			if t.CredentialID == "" {
				continue
			}
			byModel := merged[t.CredentialID]
			if byModel == nil {
				byModel = map[string]*subagentadvice.ModelUsage{}
				merged[t.CredentialID] = byModel
			}
			u := byModel[t.Model]
			if u == nil {
				u = &subagentadvice.ModelUsage{Model: t.Model}
				byModel[t.Model] = u
			}
			u.Requests += int64(t.Attempts)
			u.InputTokens += t.InputTokens - t.CacheReadTokens
			u.OutputTokens += t.OutputTokens
			u.CacheReadTokens += t.CacheReadTokens
		}
		out := map[string][]subagentadvice.ModelUsage{}
		for id, byModel := range merged {
			for _, u := range byModel {
				if u.InputTokens < 0 {
					u.InputTokens = 0
				}
				out[id] = append(out[id], *u)
			}
		}
		return out, nil
	}
	day, errDay := collect(now.Add(-24 * time.Hour))
	recent, errRecent := collect(now.Add(-3 * time.Hour))
	if errDay != nil || errRecent != nil {
		log.WithField("component", "subagent-advisor").Debug("usage history unavailable for forecasting")
		return nil
	}
	result := map[string]*subagentadvice.UsageHistory{}
	for id, list := range day {
		result[id] = &subagentadvice.UsageHistory{Last24h: list, Last3h: recent[id]}
	}
	r.mu.Lock()
	r.usage, r.usageAt = result, now
	r.mu.Unlock()
	return result
}

// allowanceHistory returns durable weekly observations from the last day.
func (r *subagentInventory) allowanceHistory(ctx context.Context, id string, now time.Time) []subagentadvice.Sample {
	a := r.archive()
	if a == nil {
		return nil
	}
	r.mu.Lock()
	if at, ok := r.allowAt[id]; ok && now.Sub(at) < subagentLedgerTTL {
		cached := r.allow[id]
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()
	var samples []subagentadvice.Sample
	err := a.Allowance(accountpolicyledger.Query{CredentialID: id, From: now.Add(-24 * time.Hour), To: now}, claudeLongWindowSeconds, func(o accountpolicyledger.Observation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if o.Model == "" {
			samples = append(samples, subagentadvice.Sample{At: o.ObservedAt, UsedPercent: o.UsedPercent, ResetAt: o.ResetAt})
		}
		return nil
	})
	if err != nil {
		samples = nil
	}
	r.mu.Lock()
	r.allowAt[id], r.allow[id] = now, samples
	r.mu.Unlock()
	return samples
}

func subagentAdviceFailure(c *gin.Context, err error) {
	var e *subagentadvice.Error
	if errors.As(err, &e) {
		c.AbortWithStatusJSON(e.Status, gin.H{"error": gin.H{"code": e.Code, "message": e.Message}})
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "subagent_advice_failed", "message": "Subagent advice failed."}})
}

// queryInt reads an optional non-negative integer query parameter.
func queryInt(c *gin.Context, name string) (int, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// registerSubagentAdviceRoutes mounts the advice endpoint without a key: it is
// read-only and publishes only model advice and coarse provider readiness. The
// per-account forecast is diagnostic detail and stays behind inference auth.
func (s *Server) registerSubagentAdviceRoutes(authenticated *gin.RouterGroup) {
	s.engine.GET(subagentAdvicePath, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if s.subagentAdvisor == nil {
			subagentAdviceFailure(c, &subagentadvice.Error{Code: "unavailable", Message: "Subagent advisor unavailable.", Status: http.StatusServiceUnavailable})
			return
		}
		contextTokens, okContext := queryInt(c, "context_tokens")
		if !okContext {
			subagentAdviceFailure(c, &subagentadvice.Error{Code: "invalid_request", Message: "context_tokens must be a non-negative integer.", Status: http.StatusBadRequest})
			return
		}
		format := strings.TrimSpace(c.Query("format"))
		if format != "" && format != "json" && format != "text" {
			subagentAdviceFailure(c, &subagentadvice.Error{Code: "invalid_request", Message: "format must be json or text.", Status: http.StatusBadRequest})
			return
		}
		advice, err := s.subagentAdvisor.Advise(c.Request.Context(), subagentadvice.Request{ContextTokens: contextTokens})
		if err != nil {
			subagentAdviceFailure(c, err)
			return
		}
		if format == "text" {
			c.String(http.StatusOK, advice.Text())
			return
		}
		c.JSON(http.StatusOK, advice)
	})
	authenticated.GET(strings.TrimPrefix(subagentForecastPath, "/v1/account-policy"), func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if s.subagentAdvisor == nil {
			subagentAdviceFailure(c, &subagentadvice.Error{Code: "unavailable", Message: "Subagent advisor unavailable.", Status: http.StatusServiceUnavailable})
			return
		}
		contextTokens, ok := queryInt(c, "context_tokens")
		if !ok {
			subagentAdviceFailure(c, &subagentadvice.Error{Code: "invalid_request", Message: "context_tokens must be a non-negative integer.", Status: http.StatusBadRequest})
			return
		}
		forecast, err := s.subagentAdvisor.Forecast(c.Request.Context(), contextTokens)
		if err != nil {
			subagentAdviceFailure(c, err)
			return
		}
		c.JSON(http.StatusOK, forecast)
	})
}

// startSubagentSampler records quota observations periodically so usage
// pace is measurable between advice requests. It runs once per server; Stop
// cancels subagentCtx.
func (s *Server) startSubagentSampler() {
	if s.subagentAdvisor == nil || s.subagentCtx == nil {
		return
	}
	s.subagentOnce.Do(func() {
		ctx := s.subagentCtx
		go func() {
			sample := func() {
				if err := s.subagentAdvisor.Sample(ctx); err != nil {
					log.WithField("component", "subagent-advisor").Debug("quota sample skipped")
				}
			}
			sample()
			ticker := time.NewTicker(subagentSampleInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sample()
				}
			}
		}()
	})
}
