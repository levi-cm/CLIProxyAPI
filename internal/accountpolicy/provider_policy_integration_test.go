package accountpolicy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real adapter snapshots must be eligible and use structured IDs/types, never fixture-only statuses.
func TestCodexDiscoveryFeedsPolicyEvaluation(t *testing.T) {
	usage, credits := codexFixture(t, "usage"), codexFixture(t, "credits")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, usage)
		} else {
			fmt.Fprint(w, credits)
		}
	}))
	defer server.Close()
	snapshot, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	// Pin local acquisition timestamps to the scenario clock while preserving every backend business field.
	now := testTime()
	snapshot.ObservedAt = now
	snapshot.InventoryObservedAt = now
	for i := range snapshot.Buckets {
		snapshot.Buckets[i].ObservedAt = now
	}
	evaluation := Evaluate(snapshot, "other-model", settings, now)
	if !evaluation.Eligible || !evaluation.Known || evaluation.Reason != "expiring_reset_credit" || !evaluation.Deadline.Equal(time.Date(2026, 10, 5, 4, 8, 0, 0, time.UTC)) {
		t.Fatalf("actual provider snapshot rejected or ignored expiry: %+v", evaluation)
	}
	if Evaluate(snapshot, "fixture-model", settings, now).Eligible {
		t.Fatal("actual provider model restriction ignored")
	}
	settings.Automation = "off"
	evaluation = Evaluate(snapshot, "other-model", settings, now)
	if !evaluation.Eligible || evaluation.Reason != "normal_weekly_reset" || !evaluation.Deadline.Equal(time.Unix(1791496800, 0)) {
		t.Fatalf("actual provider weekly observation ignored: %+v", evaluation)
	}
}

type scenarioCodexProvider struct {
	client *CodexClient
	now    time.Time
}

func (p scenarioCodexProvider) Discover(ctx context.Context, id Identity) (Snapshot, error) {
	snapshot, err := p.client.Discover(ctx, id)
	snapshot.ObservedAt = p.now
	snapshot.InventoryObservedAt = p.now
	for i := range snapshot.Buckets {
		snapshot.Buckets[i].ObservedAt = p.now
	}
	return snapshot, err
}
func (p scenarioCodexProvider) Consume(ctx context.Context, id Identity, requestID, creditID string) (ConsumeResult, error) {
	return p.client.Consume(ctx, id, requestID, creditID)
}

func TestCodexAggregateDenialDoesNotSpendCreditForShortOnlyExhaustion(t *testing.T) {
	usage := strings.Replace(codexFixture(t, "usage"), `"allowed": true`, `"allowed": false`, 1)
	usage = strings.Replace(usage, `"used_percent": 25`, `"used_percent": 100`, 1)
	usage = strings.Replace(usage, `"used_percent": 60`, `"used_percent": 20`, 1)
	verifyCodexDoesNotSpendBeforeGuard(t, usage)
}

func TestCodexExplicitPermissionAtRoundedWeekly100DoesNotSpendBeforeGuard(t *testing.T) {
	usage := strings.Replace(codexFixture(t, "usage"), `"used_percent": 60`, `"used_percent": 100`, 1)
	verifyCodexDoesNotSpendBeforeGuard(t, usage)
}

func verifyCodexDoesNotSpendBeforeGuard(t *testing.T, usage string) {
	t.Helper()
	credits := strings.Replace(codexFixture(t, "credits"), "2026-10-05T04:18:00Z", "2026-10-05T00:00:00Z", 1)
	var consumes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage"):
			fmt.Fprint(w, usage)
		case strings.HasSuffix(r.URL.Path, "/consume"):
			consumes.Add(1)
			fmt.Fprint(w, `{"code":"nothing_to_reset","windows_reset":0}`)
		default:
			fmt.Fprint(w, credits)
		}
	}))
	defer server.Close()
	now := testTime()
	provider := scenarioCodexProvider{client: codexTestClient(server), now: now}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	settings.StateDir = t.TempDir()
	s, err := NewService(Options{Settings: settings, Provider: provider, Accounts: func() []Identity { return []Identity{codexIdentity()} }, Now: func() time.Time { return now }, IsIdle: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot("credential-a")
	if Evaluate(snapshot, "other-model", settings, now).Eligible {
		t.Fatal("short-window aggregate denial failed to block inference")
	}
	if err = s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if consumes.Load() != 0 || len(s.Operations()) != 0 {
		t.Fatalf("short-only exhaustion redeemed a weekly credit before guard: writes=%d operations=%+v", consumes.Load(), s.Operations())
	}
}

func TestCodexConsumeSchemaFailureDisablesFurtherWritesDurably(t *testing.T) {
	usage := codexFixture(t, "usage")
	credits := codexFixture(t, "credits")
	var consumes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage"):
			fmt.Fprint(w, usage)
		case strings.HasSuffix(r.URL.Path, "/consume"):
			consumes.Add(1)
			fmt.Fprint(w, `{"unexpected":true}`)
		default:
			fmt.Fprint(w, credits)
		}
	}))
	defer server.Close()
	now := testTime()
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	settings.StateDir = t.TempDir()
	opts := Options{Settings: settings, Provider: scenarioCodexProvider{client: codexTestClient(server), now: now}, Accounts: func() []Identity { return []Identity{codexIdentity()} }, Now: func() time.Time { return now }, IsIdle: func(string) bool { return true }}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Redeem(context.Background(), "credential-a", "fixture-credit-first")
	if err == nil || first.State != "outcome_unknown" || consumes.Load() != 1 {
		t.Fatal("malformed success not retained as uncertain logical operation")
	}
	now = now.Add(time.Minute)
	opts.Provider = scenarioCodexProvider{client: codexTestClient(server), now: now}
	s, err = NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background(), "credential-a"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot("credential-a")
	if !snapshot.WritesDisabled {
		t.Fatal("successful usage GET erased permanent consume schema block")
	}
	_ = s.Tick(context.Background())
	if consumes.Load() != 1 || len(s.Operations()) != 1 || s.Operations()[0].RequestID != first.RequestID {
		t.Fatal("permanent contract failure retried a provider write")
	}
}
