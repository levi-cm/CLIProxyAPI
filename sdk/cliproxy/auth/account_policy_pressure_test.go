package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type pressureSource struct {
	*policyTestSource
	enabled bool
}

func (p *pressureSource) RoutingPressure(provider string) (string, []accountpolicy.RoutingShare) {
	if !p.enabled || provider != "codex" {
		return "", nil
	}
	return "manual-pressure", []accountpolicy.RoutingShare{{CredentialID: "a"}, {CredentialID: "b"}}
}

func TestManualPressureUsesLiveLeasesAndEarlierTieBreak(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &pressureSource{p, true}
	m := NewManager(nil, s, nil)
	var leases []*policyRequestLease
	t.Cleanup(func() {
		for _, l := range leases {
			l.release()
		}
	})
	for i, want := range []string{"a", "a", "b", "a", "b"} {
		opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		leases = append(leases, l)
		got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("selection %d: %v %v, want %s", i, got, err, want)
		}
		if err := beginPolicyAttempt(context.Background(), opts, got.ID); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := m.PolicyRuntime("a").ActiveRequests, m.PolicyRuntime("b").ActiveRequests; a != 3 || b != 2 {
		t.Fatalf("counts %d/%d", a, b)
	}
	// Finish the long earlier-account attempts: cumulative request totals must not
	// force later-account work or catch-up after actual concurrent pressure ends.
	for _, i := range []int{0, 1, 3} {
		leases[i].release()
	}
	opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	leases = append(leases, l)
	got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("ignored completion: %v %v", got, err)
	}
}

func TestManualPressureBurstReservesBeforeExecution(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &pressureSource{p, true}
	m := NewManager(nil, s, nil)
	leases := make([]*policyRequestLease, 101)
	var wg sync.WaitGroup
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			leases[i] = l
			got, err := s.Pick(context.Background(), "codex", fmt.Sprintf("model-%d", i%3), opts, auths)
			if err != nil || got == nil {
				t.Errorf("pick: %v", err)
			}
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	if a, b := m.PolicyRuntime("a").ActiveRequests, m.PolicyRuntime("b").ActiveRequests; a != 51 || b != 50 {
		t.Fatalf("unreserved burst: %d/%d", a, b)
	}
}

func TestDefaultNeverSpreadsForCloseRenewalsOrUnevenAllowance(t *testing.T) {
	for _, hours := range []time.Duration{95 * time.Hour, 96 * time.Hour, 96*time.Hour + time.Nanosecond, 120 * time.Hour} {
		for _, gap := range []time.Duration{time.Nanosecond, time.Minute, 4 * time.Hour, 4*time.Hour + time.Nanosecond} {
			t.Run(fmt.Sprintf("%s/%s", hours, gap), func(t *testing.T) {
				s, p, auths, now := deadlineFixture(t)
				a, b := p.snapshots["a"], p.snapshots["b"]
				a.Buckets[0].ResetAt = now.Add(hours)
				b.Buckets[0].ResetAt = now.Add(hours + gap)
				a.Buckets[0].UsedPercent = 1
				b.Buckets[0].UsedPercent = 99
				p.snapshots["a"], p.snapshots["b"] = a, b
				m := NewManager(nil, s, nil)
				for range 20 {
					opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
					defer l.release()
					got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
					if err != nil || got.ID != "a" {
						t.Fatalf("spread without authority: %v %v", got, err)
					}
					l.selectAuth(got.ID)
				}
			})
		}
	}
}

func TestStaleEvidenceDoesNotEnableRoundRobinSpreading(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	p.settings.Fallback = "round-robin"
	auths[0].Attributes["priority"] = "0"
	for _, id := range []string{"a", "b"} {
		v := p.snapshots[id]
		v.ObservedAt = now.Add(-3 * time.Minute)
		v.Buckets[0].ObservedAt = v.ObservedAt
		p.snapshots[id] = v
	}
	for range 10 {
		got, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil || got.ID != "a" {
			t.Fatalf("staleness spread work away from last-known earlier renewal: %v %v", got, err)
		}
	}
	// Once the old renewal passes it cannot rank requests as a future renewal.
	now = now.Add(97 * time.Hour)
	s.now = func() time.Time { return now }
	got, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("past renewal stayed preferred: %v %v", got, err)
	}
}

func TestSpreadingKeepsReplayableBoundOwners(t *testing.T) {
	for _, mode := range []string{"pressure", "split"} {
		t.Run(mode, func(t *testing.T) {
			s, p, auths, _ := deadlineFixture(t)
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"bound"}}, OriginalRequest: []byte(`{"input":"self-contained"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client"}}
			got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("initial: %v %v", got, err)
			}
			if mode == "pressure" {
				s.policy = &pressureSource{p, true}
			} else {
				s.policy = &splitSource{p, "manual", []accountpolicy.RoutingShare{{CredentialID: "a", Percent: 1}, {CredentialID: "b", Percent: 99}}}
			}
			for range 10 {
				got, err = s.Pick(context.Background(), "codex", "model", opts, auths)
				if err != nil || got.ID != "a" {
					t.Fatalf("spreading moved owner: %v %v", got, err)
				}
			}
		})
	}
}

func TestManualPressureRechecksEvidenceAndAuthorityPerSelection(t *testing.T) {
	for _, change := range []string{"stale", "expired_renewal", "cancelled", "missing_lease", "reset_reservation"} {
		t.Run(change, func(t *testing.T) {
			s, p, auths, now := deadlineFixture(t)
			source := &pressureSource{p, true}
			s.policy = source
			m := NewManager(nil, s, nil)
			// Demonstrated concurrent pressure on the earlier account.
			for range 2 {
				_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				l.selectAuth("a")
				defer l.release()
			}
			opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			defer l.release()
			switch change {
			case "stale":
				b := p.snapshots["b"]
				b.ObservedAt = now.Add(-3 * time.Minute)
				p.snapshots["b"] = b
			case "expired_renewal":
				b := p.snapshots["b"]
				b.Buckets[0].ResetAt = now
				p.snapshots["b"] = b
			case "cancelled":
				source.enabled = false
			case "missing_lease":
				opts = cliproxyexecutor.Options{}
			case "reset_reservation":
				release, err := m.AcquirePolicyReset(context.Background(), "b")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || got == nil || got.ID != "a" {
				t.Fatalf("pressure survived %s: %v %v", change, got, err)
			}
		})
	}
}

func TestManualPressureReservationProtectsStreamAndResetBoundary(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &pressureSource{p, true}
	m := NewManager(nil, s, nil)
	opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	defer l.release()
	got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("pick: %v %v", got, err)
	}
	if release, err := m.AcquirePolicyReset(context.Background(), "a"); err == nil {
		release()
		t.Fatal("reset raced reserved selection")
	}
	l.streaming = true
	finishPolicyAttempt(opts)
	if m.PolicyRuntime("a").ActiveRequests != 1 {
		t.Fatal("stream released before completion")
	}
	l.release()
	release, err := m.AcquirePolicyReset(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if m.PolicyRuntime("a").ActiveRequests != 0 {
		t.Fatal("leaked completed reservation")
	}
}

func TestManualPressure429RespectsRetryAfterAndModelSupport(t *testing.T) {
	withQuotaCooldownEnabled(t)
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &pressureSource{p, true}
	m := NewManager(nil, s, nil)
	m.SetRetryConfig(5, 5*time.Second, 6)
	executor := &authFallbackExecutor{id: "codex", executeErrors: map[string]error{"a": &retryAfterStatusError{status: 429, message: "rate limited", retryAfter: time.Hour}}}
	m.RegisterExecutor(executor)
	const model = "pressure-429-model"
	for _, a := range auths {
		if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
		defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	}
	for range 2 {
		if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	calls := executor.ExecuteCalls()
	if len(calls) != 3 || calls[0] != "a" || calls[1] != "b" || calls[2] != "b" {
		t.Fatalf("429 retry storm or unsafe failover: %v", calls)
	}
	cooled, _ := m.GetByID("a")
	if !cooled.ModelStates[model].NextRetryAfter.After(time.Now().Add(59 * time.Minute)) {
		t.Fatal("Retry-After shortened")
	}
	if m.PolicyRuntime("a").ActiveRequests != 0 || m.PolicyRuntime("b").ActiveRequests != 0 {
		t.Fatal("attempt leaked reservation")
	}
	registry.GetGlobalRegistry().RegisterClient("b", "codex", []*registry.ModelInfo{{ID: model}, {ID: "only-pressure-b"}})
	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "only-pressure-b"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatal(err)
	}
	calls = executor.ExecuteCalls()
	if calls[len(calls)-1] != "b" {
		t.Fatal("unsupported model routed to earlier account")
	}
}
