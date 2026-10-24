package auth

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type policyBurnSample struct {
	at   time.Time
	used float64
}
type policyBurnHistory struct {
	identity     accountpolicy.Identity
	epoch        uint64
	renewal      time.Time
	scope, model string
	anchors      []policyBurnSample
	latest       policyBurnSample
}
type automaticSpreadingState struct {
	mu        sync.Mutex
	histories map[string]policyBurnHistory
}

func (s *automaticSpreadingState) clear() {
	s.mu.Lock()
	s.histories = nil
	s.mu.Unlock()
}

// Evidence must identify one ordinary weekly allowance. Per-model or duplicate
// windows cannot be combined into an invented consumption measurement.
func (s *EarliestDeadlineSelector) automaticEvidence(a *Auth, model string, e accountpolicy.Evaluation, now time.Time) (accountpolicy.Snapshot, accountpolicy.Bucket, bool) {
	snapshot, ok := s.policy.Snapshot(a.ID)
	settings := s.policy.Settings()
	fresh := func(at time.Time) bool {
		return !at.IsZero() && !at.After(now) && now.Sub(at) <= time.Duration(settings.FreshnessSeconds)*time.Second
	}
	if !ok || !e.Eligible || !e.Known || e.Reason != "normal_weekly_reset" || snapshot.Identity.CredentialID != a.ID || snapshot.Identity.Provider != "codex" || snapshot.Identity.AccountID == "" || !snapshot.InventoryComplete || !fresh(snapshot.ObservedAt) || !fresh(snapshot.InventoryObservedAt) || snapshot.LastError != "" {
		return snapshot, accountpolicy.Bucket{}, false
	}
	quotaModel := canonicalModelKey(model)
	s.mu.Lock()
	resolve := s.modelResolver
	s.mu.Unlock()
	if resolve != nil {
		quotaModel = resolve(a, model)
	}
	current := accountpolicy.Evaluate(snapshot, quotaModel, settings, now)
	if !current.Eligible || !current.Known || current.Reason != "normal_weekly_reset" || !current.Deadline.Equal(e.Deadline) {
		return snapshot, accountpolicy.Bucket{}, false
	}
	var weekly accountpolicy.Bucket
	count := 0
	for _, b := range snapshot.Buckets {
		if (b.Scope == "ordinary" || b.Scope == "") && (b.Model == "" || b.Model == quotaModel) && b.DurationSeconds == 604800 {
			if !fresh(b.ObservedAt) || math.IsNaN(b.UsedPercent) || math.IsInf(b.UsedPercent, 0) || b.UsedPercent < 0 || b.UsedPercent >= 100 || b.Allowed != nil && !*b.Allowed {
				return snapshot, weekly, false
			}
			weekly = b
			count++
		}
	}
	return snapshot, weekly, count == 1 && weekly.ResetAt.Equal(e.Deadline)
}

// Observe immutable provider percentages at selection time. This private cache
// warms cold after restart, keeps at most thirteen five-minute anchors per
// account, and never reads/writes the durable history archive or polls providers.
func (s *automaticSpreadingState) observe(a *Auth, snapshot accountpolicy.Snapshot, bucket accountpolicy.Bucket) policyBurnHistory {
	if s.histories == nil {
		s.histories = make(map[string]policyBurnHistory)
	}
	h := s.histories[a.ID]
	sample := policyBurnSample{bucket.ObservedAt, bucket.UsedPercent}
	changed := h.identity != snapshot.Identity || h.epoch != a.RegistrationEpoch || !h.renewal.Equal(bucket.ResetAt) || h.scope != bucket.Scope || h.model != bucket.Model
	invalid := !h.latest.at.IsZero() && (sample.at.Before(h.latest.at) || sample.used < h.latest.used || sample.at.Sub(h.latest.at) > 10*time.Minute || sample.at.Equal(h.latest.at) && sample.used != h.latest.used)
	if changed || invalid || len(h.anchors) == 0 {
		h = policyBurnHistory{identity: snapshot.Identity, epoch: a.RegistrationEpoch, renewal: bucket.ResetAt, scope: bucket.Scope, model: bucket.Model, anchors: []policyBurnSample{sample}, latest: sample}
	} else if sample.at.After(h.latest.at) {
		h.latest = sample
		if sample.at.Sub(h.anchors[len(h.anchors)-1].at) >= 5*time.Minute {
			h.anchors = append(h.anchors, sample)
			if len(h.anchors) > 13 {
				h.anchors = append([]policyBurnSample(nil), h.anchors[len(h.anchors)-13:]...)
			}
		}
	}
	s.histories[a.ID] = h
	return h
}

// This is a forecast, not a guarantee of future demand. Use the slowest observed
// allowance-burn interval, a 75% rate haircut, a 24-hour margin and 2x coverage.
// A partial latest interval can veto spreading immediately when burn slows.
func (h policyBurnHistory) forecastCovers(now time.Time) bool {
	if len(h.anchors) < 13 || h.anchors[len(h.anchors)-1].at.Sub(h.anchors[0].at) < time.Hour {
		return false
	}
	rate := math.Inf(1)
	previous := h.anchors[0]
	for _, sample := range h.anchors[1:] {
		hours := sample.at.Sub(previous.at).Hours()
		if hours <= 0 || hours > 1.0/6 {
			return false
		}
		rate = math.Min(rate, (sample.used-previous.used)/hours)
		previous = sample
	}
	if h.latest.at.After(previous.at) {
		rate = math.Min(rate, (h.latest.used-previous.used)/h.latest.at.Sub(previous.at).Hours())
	}
	remaining := 100 - h.latest.used
	horizon := h.renewal.Sub(now) - 24*time.Hour
	return rate > 0 && !math.IsInf(rate, 0) && remaining > 0 && horizon > 0 && rate/4*horizon.Hours() >= 2*remaining
}

// Automatic spreading has no sticky mode: every new selection checks all gates.
// Missing/stale/ambiguous evidence discards forecasts, preserving deadline order.
func (s *EarliestDeadlineSelector) pickAutomatic(provider, model string, opts cliproxyexecutor.Options, eligible []*Auth, evaluations map[string]accountpolicy.Evaluation) *Auth {
	reject := func() *Auth { s.automatic.clear(); return nil }
	splitID, _ := s.routingSplit(provider)
	pressureID, _ := s.routingPressure(provider)
	if provider != "codex" || len(eligible) != 2 || s.preferredAccount(provider) != "" || splitID != "" || pressureID != "" {
		return reject()
	}
	candidates := append([]*Auth(nil), eligible...)
	sort.Slice(candidates, func(i, j int) bool { return earlierPolicyAuth(candidates[i], candidates[j], evaluations) })
	now := s.now()
	var snapshots [2]accountpolicy.Snapshot
	var buckets [2]accountpolicy.Bucket
	for i, a := range candidates {
		snapshot, bucket, ok := s.automaticEvidence(a, model, evaluations[a.ID], now)
		if !ok || bucket.ResetAt.Sub(now) <= 96*time.Hour {
			return reject()
		}
		snapshots[i], buckets[i] = snapshot, bucket
	}
	if buckets[1].ResetAt.Sub(buckets[0].ResetAt) > 4*time.Hour || snapshots[0].Identity.AccountID == snapshots[1].Identity.AccountID && snapshots[0].Identity.WorkspaceID == snapshots[1].Identity.WorkspaceID {
		return reject()
	}
	s.automatic.mu.Lock()
	// Forget replaced pairs rather than accumulating histories for retired auths.
	for id := range s.automatic.histories {
		if id != candidates[0].ID && id != candidates[1].ID {
			delete(s.automatic.histories, id)
		}
	}
	earlier := s.automatic.observe(candidates[0], snapshots[0], buckets[0])
	s.automatic.observe(candidates[1], snapshots[1], buckets[1])
	covers := earlier.forecastCovers(now)
	s.automatic.mu.Unlock()
	if !covers {
		return nil
	}
	lease, ok := opts.Metadata[policyRequestLeaseMetadataKey].(*policyRequestLease)
	if !ok || lease == nil {
		return nil
	}
	return lease.reservePressure(candidates, 4)
}
