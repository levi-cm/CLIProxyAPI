package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Warm with actual provider percentage deltas, never a request/token count.
func warmAutomaticForecast(t *testing.T, burnPerStep float64) (*EarliestDeadlineSelector, *policyTestSource, []*Auth, *Manager, *time.Time) {
	t.Helper()
	return warmAutomaticForecastWithRenewals(t, burnPerStep, 143*time.Hour, 2*time.Hour)
}

func warmAutomaticForecastWithRenewals(t *testing.T, burnPerStep float64, untilRenewal, gap time.Duration) (*EarliestDeadlineSelector, *policyTestSource, []*Auth, *Manager, *time.Time) {
	t.Helper()
	s, p, auths, start := deadlineFixture(t)
	now := start
	s.now = func() time.Time { return now }
	renewal := start.Add(untilRenewal + time.Hour)
	m := NewManager(nil, s, nil)
	for i := 0; i <= 12; i++ {
		now = start.Add(time.Duration(i) * 5 * time.Minute)
		for _, a := range auths {
			snapshot := p.snapshots[a.ID]
			snapshot.Buckets = append([]accountpolicy.Bucket(nil), snapshot.Buckets...)
			snapshot.ObservedAt = now
			snapshot.InventoryObservedAt = now
			snapshot.Buckets[0].ObservedAt = now
			snapshot.Buckets[0].ResetAt = renewal
			snapshot.Buckets[0].UsedPercent = 10 + float64(i)*burnPerStep
			if a.ID == "b" {
				snapshot.Buckets[0].ResetAt = renewal.Add(gap)
				snapshot.Buckets[0].UsedPercent = 20
			}
			p.snapshots[a.ID] = snapshot
		}
		opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
		l.release()
		if err != nil || got.ID != "a" {
			t.Fatalf("warmup spread without pressure: %v %v", got, err)
		}
	}
	return s, p, auths, m, &now
}

func TestAutomaticPressureRequiresDemonstratedPressureAndAllowanceForecast(t *testing.T) {
	for _, burn := range []float64{0, .01, 1} {
		t.Run(fmt.Sprint(burn), func(t *testing.T) {
			s, _, auths, m, _ := warmAutomaticForecast(t, burn)
			for range 4 {
				_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				l.selectAuth("a")
				defer l.release()
			}
			opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			defer l.release()
			got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			want := "a"
			if burn == 1 {
				want = "b"
			}
			if err != nil || got.ID != want {
				t.Fatalf("burn %v: %v %v want %s", burn, got, err, want)
			}
		})
	}
}

func TestAutomaticPressureRenewalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		until, gap time.Duration
		want       string
	}{
		{"below96", 96*time.Hour - time.Nanosecond, 2 * time.Hour, "a"},
		{"exact96", 96 * time.Hour, 2 * time.Hour, "a"},
		{"above96", 96*time.Hour + time.Nanosecond, 2 * time.Hour, "b"},
		{"fourHours", 120 * time.Hour, 4 * time.Hour, "b"},
		{"beyondFourHours", 120 * time.Hour, 4*time.Hour + time.Nanosecond, "a"},
		{"oneNanosecondGap", 120 * time.Hour, time.Nanosecond, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, auths, m, _ := warmAutomaticForecastWithRenewals(t, 1, tc.until, tc.gap)
			for range 4 {
				_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				l.selectAuth("a")
				defer l.release()
			}
			opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			defer l.release()
			got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || got.ID != tc.want {
				t.Fatalf("renewal boundary: %v %v want %s", got, err, tc.want)
			}
		})
	}
}

func TestAutomaticPressureExitsOnNewSelectionWithoutPolling(t *testing.T) {
	for _, change := range []string{"stale", "stale_bucket", "stale_inventory", "refill", "renewal", "identity", "missing_inventory", "error", "unsupported_scope", "ambiguous_bucket", "zero_partial_burn", "low_partial_burn", "reset_reservation"} {
		t.Run(change, func(t *testing.T) {
			s, p, auths, m, now := warmAutomaticForecast(t, 1)
			for range 4 {
				_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				l.selectAuth("a")
				defer l.release()
			}
			pick := func() string {
				opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				defer l.release()
				got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
				if err != nil {
					t.Fatal(err)
				}
				return got.ID
			}
			if got := pick(); got != "b" {
				t.Fatalf("did not enter forecast spreading: %s", got)
			}
			a, b := p.snapshots["a"], p.snapshots["b"]
			a.Buckets = append([]accountpolicy.Bucket(nil), a.Buckets...)
			b.Buckets = append([]accountpolicy.Bucket(nil), b.Buckets...)
			switch change {
			case "stale":
				b.ObservedAt = now.Add(-121 * time.Second)
			case "stale_bucket":
				b.Buckets[0].ObservedAt = now.Add(-121 * time.Second)
			case "stale_inventory":
				b.InventoryObservedAt = now.Add(-121 * time.Second)
			case "refill":
				a.Buckets[0].UsedPercent = 1
			case "renewal":
				a.Buckets[0].ResetAt = a.Buckets[0].ResetAt.Add(time.Hour)
			case "identity":
				a.Identity.AccountID = "replacement"
			case "missing_inventory":
				b.InventoryComplete = false
			case "error":
				b.LastError = "observation failed"
			case "unsupported_scope":
				b.Buckets[0].Scope = "code_review"
			case "ambiguous_bucket":
				b.Buckets = append(b.Buckets, b.Buckets[0])
			case "zero_partial_burn", "low_partial_burn":
				*now = now.Add(time.Minute)
				a.ObservedAt = *now
				a.InventoryObservedAt = *now
				a.Buckets[0].ObservedAt = *now
				b.ObservedAt = *now
				b.InventoryObservedAt = *now
				b.Buckets[0].ObservedAt = *now
				if change == "low_partial_burn" {
					a.Buckets[0].UsedPercent += .001
				}
			case "reset_reservation":
				release, err := m.AcquirePolicyReset(context.Background(), "b")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			p.snapshots["a"], p.snapshots["b"] = a, b
			if got := pick(); got != "a" {
				t.Fatalf("continued spreading after %s: %s", change, got)
			}
		})
	}
}

func TestAutomaticBurstReservationsKeepEarlierPriorityAcrossModels(t *testing.T) {
	s, _, auths, m, _ := warmAutomaticForecast(t, 1)
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
		t.Fatalf("burst allocation: %d/%d", a, b)
	}
	// Completion changes actual pressure immediately; no later-account catch-up.
	for _, l := range leases {
		if l.authID == "a" {
			l.release()
		}
	}
	opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	defer l.release()
	got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("ignored completed early work: %v %v", got, err)
	}
}

type changingAutomaticSource struct {
	*policyTestSource
	reads  map[string]int
	change func(string, accountpolicy.Snapshot) accountpolicy.Snapshot
}

func (p *changingAutomaticSource) Snapshot(id string) (accountpolicy.Snapshot, bool) {
	snapshot, ok := p.policyTestSource.Snapshot(id)
	p.reads[id]++
	if p.reads[id] >= 3 {
		snapshot = p.change(id, snapshot)
	}
	return snapshot, ok
}

func TestAutomaticReevaluatesRefreshedSnapshotBeforeSpreading(t *testing.T) {
	for _, change := range []string{"ineligible", "disabled", "expiring_credit"} {
		t.Run(change, func(t *testing.T) {
			s, p, auths, m, now := warmAutomaticForecast(t, 1)
			for range 4 {
				_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				l.selectAuth("a")
				defer l.release()
			}
			s.policy = &changingAutomaticSource{policyTestSource: p, reads: map[string]int{}, change: func(id string, snapshot accountpolicy.Snapshot) accountpolicy.Snapshot {
				if change == "ineligible" && id == "b" {
					snapshot.Eligible = false
				}
				if change == "disabled" && id == "b" {
					snapshot.Status = "disabled"
				}
				if change == "expiring_credit" && id == "a" {
					expiry := now.Add(time.Hour)
					snapshot.Credits = []accountpolicy.Credit{{ID: "new", Type: "codex_rate_limits", Status: "available", DetailsKnown: true, Scopes: []string{"ordinary"}, ExpiresAt: &expiry}}
				}
				return snapshot
			}}
			opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			defer l.release()
			got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("spread using superseded eligibility: %v %v", got, err)
			}
		})
	}
}

func TestAutomaticStaleEvidenceRequiresNewForecastWarmup(t *testing.T) {
	s, p, auths, m, now := warmAutomaticForecast(t, 1)
	for range 4 {
		_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		l.selectAuth("a")
		defer l.release()
	}
	b := p.snapshots["b"]
	b.ObservedAt = now.Add(-121 * time.Second)
	p.snapshots["b"] = b
	for i := 0; i < 2; i++ {
		opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
		l.release()
		if err != nil || got.ID != "a" {
			t.Fatalf("reused stale forecast: %v %v", got, err)
		}
		b.ObservedAt = *now
		p.snapshots["b"] = b
	}
}

func TestAutomaticUnevenBurnCannotUseAverageConsumption(t *testing.T) {
	s, p, auths, start := deadlineFixture(t)
	now := start
	s.now = func() time.Time { return now }
	m := NewManager(nil, s, nil)
	for i := 0; i <= 12; i++ {
		now = start.Add(time.Duration(i) * 5 * time.Minute)
		for _, a := range auths {
			snapshot := p.snapshots[a.ID]
			snapshot.Buckets = append([]accountpolicy.Bucket(nil), snapshot.Buckets...)
			snapshot.ObservedAt = now
			snapshot.InventoryObservedAt = now
			snapshot.Buckets[0].ObservedAt = now
			snapshot.Buckets[0].ResetAt = start.Add(144 * time.Hour)
			if a.ID == "b" {
				snapshot.Buckets[0].ResetAt = start.Add(146 * time.Hour)
			}
			// High average burn with a demonstrated idle interval must fail the minimum.
			if a.ID == "a" {
				snapshot.Buckets[0].UsedPercent = 10 + float64(i)*4
				if i >= 6 {
					snapshot.Buckets[0].UsedPercent -= 4
				}
			}
			p.snapshots[a.ID] = snapshot
		}
		opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		if _, err := s.Pick(context.Background(), "codex", "model", opts, auths); err != nil {
			t.Fatal(err)
		}
		l.release()
	}
	for range 8 {
		_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		l.selectAuth("a")
		defer l.release()
	}
	opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	defer l.release()
	got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("average hid uneven consumption: %v %v", got, err)
	}
}

func TestAutomaticPressureKeepsBoundConversations(t *testing.T) {
	for _, body := range []string{`{"input":"self-contained"}`, `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`} {
		t.Run(body, func(t *testing.T) {
			s, _, auths, m, _ := warmAutomaticForecast(t, 1)
			base := cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"automatic-bound"}}, OriginalRequest: []byte(`{"input":"initial"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client"}}
			got, err := s.Pick(context.Background(), "codex", "model", base, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("initial owner: %v %v", got, err)
			}
			for range 4 {
				_, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				l.selectAuth("a")
				defer l.release()
			}
			base.OriginalRequest = []byte(body)
			opts, l := m.trackPolicyRequest(context.Background(), base)
			defer l.release()
			got, err = s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("automatic pressure moved bound owner: %v %v", got, err)
			}
		})
	}
}
