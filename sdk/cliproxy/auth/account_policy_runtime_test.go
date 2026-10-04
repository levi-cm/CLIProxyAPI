package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type dashboardLegacyBindingSelector struct {
	FillFirstSelector
	retained int
}

func (s *dashboardLegacyBindingSelector) ActiveBindings(string) int { s.retained = 0; return 0 }

// A custom legacy observer cannot establish read-only binding availability.
func TestPolicyRuntimeDashboardSnapshotRejectsMutatingLegacyObserver(t *testing.T) {
	s := &dashboardLegacyBindingSelector{retained: 3}
	m := NewManager(nil, s, nil)
	_, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	lease.selectAuth("a")
	defer lease.release()
	statuses, available := m.PolicyRuntimeSnapshot([]string{"a"})
	if available || s.retained != 3 || statuses["a"].ActiveRequests != 1 || statuses["a"].LastSelectedAt == nil {
		t.Fatalf("legacy observer mutated state or hid request evidence: %+v available=%v retained=%d", statuses, available, s.retained)
	}
}

// Policy disabled mode must report the configured fallback's canonical groups,
// with expired groups and aliases neither counted nor removed by the snapshot.
func TestPolicyRuntimeDashboardSnapshotDisabledFallbackSessionGroups(t *testing.T) {
	fallback := NewSessionAffinitySelector(nil)
	t.Cleanup(fallback.Stop)
	future := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	expired := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	fallback.cache.groups["fresh"] = sessionEntry{authID: "a", expiresAt: future, aliases: []string{"one", "two"}}
	fallback.cache.groups["expired"] = sessionEntry{authID: "b", expiresAt: expired, aliases: []string{"three"}}
	fallback.cache.entries["one"], fallback.cache.entries["two"] = fallback.cache.groups["fresh"], fallback.cache.groups["fresh"]
	fallback.cache.entries["three"] = fallback.cache.groups["expired"]
	source := &policyTestSource{settings: accountpolicy.DefaultSettings()}
	s := NewEarliestDeadlineSelector(source, fallback)
	t.Cleanup(s.Stop)
	s.bindings["previous-policy-binding"] = policyBinding{authID: "b", seen: time.Now()}
	m := NewManager(nil, s, nil)
	statuses, available := m.PolicyRuntimeSnapshot([]string{"a", "b"})
	if !available || statuses["a"].ActiveBindings != 1 || statuses["b"].ActiveBindings != 0 || len(fallback.cache.groups) != 2 || len(fallback.cache.entries) != 3 || len(s.bindings) != 1 {
		t.Fatalf("disabled policy sampled stale state or mutated groups: %+v", statuses)
	}
}

// A dashboard observation must exclude expired bindings without removing them.
func TestPolicyRuntimeDashboardSnapshotDoesNotExpireBindings(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s := NewEarliestDeadlineSelector(nil, nil, func() time.Time { return now })
	t.Cleanup(s.Stop)
	s.bindings["fresh"] = policyBinding{authID: "a", seen: now.Add(-time.Minute)}
	s.bindings["edge"] = policyBinding{authID: "b", seen: now.Add(-time.Hour)}
	s.bindings["expired"] = policyBinding{authID: "a", seen: now.Add(-time.Hour - time.Nanosecond)}
	m := NewManager(nil, s, nil)
	reader, ok := any(m).(interface {
		PolicyRuntimeSnapshot([]string) (map[string]PolicyRuntimeStatus, bool)
	})
	if !ok {
		t.Fatal("read-only batch runtime snapshot is unavailable")
	}
	statuses, available := reader.PolicyRuntimeSnapshot([]string{"a", "b", "c"})
	if !available || len(statuses) != 3 || statuses["a"].ActiveBindings != 1 || statuses["b"].ActiveBindings != 1 || statuses["c"].ActiveBindings != 0 {
		t.Fatalf("wrong live counts: %+v available=%v", statuses, available)
	}
	if len(s.bindings) != 3 || s.bindings["expired"].authID != "a" {
		t.Fatal("dashboard expired selector-owned affinity state")
	}
}

// One batch must sample a bounded binding map once for all account IDs.
func TestPolicyRuntimeDashboardSnapshotScalesAcrossAccounts(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clockReads := 0
	s := NewEarliestDeadlineSelector(nil, nil, func() time.Time { clockReads++; return now })
	t.Cleanup(s.Stop)
	ids := make([]string, 128)
	for i := range ids {
		ids[i] = fmt.Sprintf("account-%d", i)
	}
	for i := 0; i < defaultMaxSessionEntries; i++ {
		s.bindings[fmt.Sprintf("binding-%d", i)] = policyBinding{authID: ids[i%128], seen: now.Add(-time.Minute)}
	}
	m := NewManager(nil, s, nil)
	reader, ok := any(m).(interface {
		PolicyRuntimeSnapshot([]string) (map[string]PolicyRuntimeStatus, bool)
	})
	if !ok {
		t.Fatal("read-only batch runtime snapshot is unavailable")
	}
	statuses, available := reader.PolicyRuntimeSnapshot(ids)
	if !available || len(statuses) != 128 || clockReads != 1 {
		t.Fatalf("snapshot rescanned per account: available=%v accounts=%d clocks=%d", available, len(statuses), clockReads)
	}
	for _, id := range ids {
		if statuses[id].ActiveBindings != 512 {
			t.Fatalf("%s bindings=%d want512", id, statuses[id].ActiveBindings)
		}
	}
	if len(s.bindings) != defaultMaxSessionEntries {
		t.Fatal("snapshot changed bounded selector state")
	}
}

// A selection timestamp must be created by selection and survive completion without claiming activity.
func TestPolicyRuntimeSelectionTimestampAndConcurrentAccounts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	decode := func(id string) map[string]any {
		data, err := json.Marshal(m.PolicyRuntime(id))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	if fields := decode("a"); fields["last_selected_at"] != nil {
		t.Fatal("unselected account has selection time")
	}
	before := time.Now()
	leases := make([]*policyRequestLease, 18)
	var wg sync.WaitGroup
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, leases[i] = m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			leases[i].selectAuth([]string{"a", "b", "c"}[i%3])
		}(i)
	}
	wg.Wait()
	for _, id := range []string{"a", "b", "c"} {
		fields := decode(id)
		timestamp, ok := fields["last_selected_at"].(string)
		if !ok {
			t.Fatal("real selection missing timestamp")
		}
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || at.Before(before) || at.After(time.Now()) || m.PolicyRuntime(id).ActiveRequests != 6 {
			t.Fatalf("wrong selected activity: %s %+v", id, fields)
		}
	}
	selected := decode("a")["last_selected_at"]
	for _, lease := range leases {
		lease.release()
	}
	if m.PolicyRuntime("a").ActiveRequests != 0 || decode("a")["last_selected_at"] != selected {
		t.Fatal("completion changed selection time or left activity")
	}
}

type policyPoolLeaseExecutor struct {
	*openAICompatPoolExecutor
	manager  *Manager
	activeMu sync.Mutex
	active   []int
}

func (e *policyPoolLeaseExecutor) observe(auth *Auth) {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	e.active = append(e.active, e.manager.PolicyRuntime(auth.ID).ActiveRequests)
}

func (e *policyPoolLeaseExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.observe(auth)
	return e.openAICompatPoolExecutor.Execute(ctx, auth, req, opts)
}

func (e *policyPoolLeaseExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.observe(auth)
	return e.openAICompatPoolExecutor.CountTokens(ctx, auth, req, opts)
}

func (e *policyPoolLeaseExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.observe(auth)
	return e.openAICompatPoolExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestPolicyRuntimeReacquiresLeaseForModelPoolRetry(t *testing.T) {
	for _, transport := range []string{"http", "count", "stream"} {
		t.Run(transport, func(t *testing.T) {
			upstreamErr := &Error{HTTPStatus: http.StatusInternalServerError, Message: "first model failed"}
			base := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey,
				executeErrors:     map[string]error{"first": upstreamErr},
				countErrors:       map[string]error{"first": upstreamErr},
				streamFirstErrors: map[string]error{"first": upstreamErr},
			}
			manager := newOpenAICompatPoolTestManager(t, "public", []internalconfig.OpenAICompatibilityModel{{Name: "first", Alias: "public"}, {Name: "second", Alias: "public"}}, base)
			executor := &policyPoolLeaseExecutor{openAICompatPoolExecutor: base, manager: manager}
			manager.RegisterExecutor(executor)
			request := cliproxyexecutor.Request{Model: "public"}
			var err error
			switch transport {
			case "http":
				_, err = manager.Execute(context.Background(), []string{openAICompatPoolProviderKey}, request, cliproxyexecutor.Options{})
			case "count":
				_, err = manager.ExecuteCount(context.Background(), []string{openAICompatPoolProviderKey}, request, cliproxyexecutor.Options{})
			case "stream":
				var stream *cliproxyexecutor.StreamResult
				stream, err = manager.ExecuteStream(context.Background(), []string{openAICompatPoolProviderKey}, request, cliproxyexecutor.Options{})
				if err == nil {
					readOpenAICompatStreamPayload(t, stream)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			executor.activeMu.Lock()
			defer executor.activeMu.Unlock()
			if len(executor.active) != 2 || executor.active[0] != 1 || executor.active[1] != 1 {
				t.Fatalf("active inference leases = %v, want [1 1]", executor.active)
			}
		})
	}
}

// Selection must keep the account busy until the real response boundary.
func TestPolicyRuntimeLeaseProtectsAccountUntilRelease(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex"}
	m.auths["b"] = &Auth{ID: "b", Provider: "codex"}
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	if !m.PolicyIsIdle("a") {
		t.Fatal("account unexpectedly busy before selection")
	}
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "a", Provider: "codex"})
	if m.PolicyIsIdle("a") || !m.PolicyHasDemand("a") {
		t.Fatal("selected account must remain busy")
	}
	if got := m.PolicyRuntime("a").ActiveRequests; got != 1 {
		t.Fatalf("active=%d, want 1", got)
	}
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "b", Provider: "codex"})
	if !m.PolicyIsIdle("a") || m.PolicyIsIdle("b") {
		t.Fatal("retry must transfer accounting after previous attempt ended")
	}
	lease.release()
	lease.release()
	if !m.PolicyIsIdle("b") {
		t.Fatal("completed account remains busy")
	}
}

type policyProxyCapture struct{ proxy string }

func (p *policyProxyCapture) RoundTripperFor(a *Auth) http.RoundTripper {
	p.proxy = a.ProxyURL
	return http.DefaultTransport
}

func TestPolicyCredentialTransportPreservesGlobalProxy(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex"}
	cfg := &internalconfig.Config{}
	cfg.ProxyURL = "http://127.0.0.1:12345"
	m.SetConfig(cfg)
	provider := &policyProxyCapture{}
	m.SetRoundTripperProvider(provider)
	if m.CredentialRoundTripper("a") == nil || provider.proxy != "http://127.0.0.1:12345" {
		t.Fatalf("discovery bypassed global proxy %q", provider.proxy)
	}
	if m.auths["a"].ProxyURL != "" {
		t.Fatal("transport lookup mutated credential")
	}
}

func TestPolicyResetReservationRejectsBusyAndBlocksNewSelection(t *testing.T) {
	m := NewManager(nil, nil, nil)
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "a"})
	if _, err := m.AcquirePolicyReset(context.Background(), "a"); err == nil {
		t.Fatal("reset admitted active inference")
	}
	lease.release()
	release, err := m.AcquirePolicyReset(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitingOpts, waitingLease := m.trackPolicyRequest(ctx, cliproxyexecutor.Options{})
	publishSelectedAuthMetadata(waitingOpts.Metadata, &Auth{ID: "a"})
	if m.PolicyRuntime("a").ActiveRequests != 0 {
		t.Fatal("cancelled selection entered reserved account")
	}
	waitingLease.release()
	release()
	release()
	if _, err = m.AcquirePolicyReset(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.AcquirePolicyReset(ctx, "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire=%v", err)
	}
}

func TestPendingPolicyDemandCanWaitWhileAccountIdle(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex"}
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{}, policyRequestDemand{providers: []string{"codex"}})
	if !m.PolicyHasDemand("a") || !m.PolicyIsIdle("a") {
		t.Fatal("waiting demand must be visible while account has no active inference")
	}
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "a", Provider: "codex"})
	lease.finishAttempt()
	if !m.PolicyHasDemand("a") || !m.PolicyIsIdle("a") {
		t.Fatal("failed attempt awaiting retry lost pending demand")
	}
	lease.release()
	if m.PolicyHasDemand("a") {
		t.Fatal("completed request left demand")
	}
}
