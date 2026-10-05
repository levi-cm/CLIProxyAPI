package accountpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func observationSettings(t *testing.T) Settings {
	t.Helper()
	settings := DefaultSettings()
	if err := json.Unmarshal([]byte(`{"observations_enabled":true}`), &settings); err != nil {
		t.Fatal(err)
	}
	settings.StateDir = filepath.Join(t.TempDir(), "observations")
	return settings
}

// Observation-only reads must retain provider retry delays even after refresh events.
func TestObservationOnlyHonorsDiscoveryBackoff(t *testing.T) {
	now := testTime()
	snapshot := testSnapshot("b", 7)
	reads := 0
	provider := &fakeProvider{discover: func(Identity) (Snapshot, error) {
		reads++
		return Snapshot{}, delayedError{}
	}}
	service, err := NewService(Options{Settings: observationSettings(t), Provider: provider, Now: func() time.Time { return now }, Accounts: func() []Identity { return []Identity{snapshot.Identity} }})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Tick(context.Background()); err == nil || reads != 1 {
		t.Fatalf("failed observation was not attempted: reads=%d err=%v", reads, err)
	}
	for i := 0; i < 5; i++ {
		service.RequestRefresh("b")
		now = now.Add(time.Second)
		if err = service.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatalf("refresh events bypassed provider retry delay: %d reads", reads)
	}
	now = testTime().Add(5 * time.Minute)
	if err = service.Tick(context.Background()); err == nil || reads != 2 {
		t.Fatalf("due retry was not attempted once: reads=%d err=%v", reads, err)
	}
	observed, ok := service.Snapshot("b")
	if !ok || observed.LastError == "" || !observed.ObservedAt.IsZero() {
		t.Fatalf("discovery failure invented fresh quota: %+v", observed)
	}
}

// Disabling routing must block every write path even with exhausted quota and expiring credits.
func TestObservationOnlyNeverConsumesOrRecovers(t *testing.T) {
	now := testTime()
	snapshot := testSnapshot("b", 7)
	expires := now.Add(9 * time.Minute)
	snapshot.Credits = []Credit{testCredit("credit", &expires)}
	snapshot.AvailableCredits = 1
	snapshot.Buckets[1].UsedPercent = 100
	provider := &fakeProvider{snapshots: map[string]Snapshot{"b": snapshot}}
	settings := observationSettings(t)
	settings.Automation = "auto_expiring"
	recoveries := 0
	service, err := NewService(Options{Settings: settings, Provider: provider, Now: func() time.Time { return now }, Accounts: func() []Identity { return []Identity{snapshot.Identity} }, HasDemand: func(string) bool { return true }, IsIdle: func(string) bool { return true }, Recover: func(context.Context, Snapshot, Snapshot) error { recoveries++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := service.Snapshot("b"); !ok {
		t.Fatal("observation-only mode did not read exhausted quota")
	}
	service.mu.Lock()
	service.state.Schedules = []Schedule{{ID: "due", CredentialID: "b", AccountID: "b", WorkspaceID: "b", Provider: "codex", CreditID: "credit", At: now}}
	service.state.Recoveries = map[string]recoveryRecord{"b": {Before: clone(snapshot), After: clone(snapshot)}}
	service.mu.Unlock()
	if err = service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Redeem(context.Background(), "b", "credit"); err == nil {
		t.Fatal("observation-only mode accepted a redemption")
	}
	provider.snapshots["b"] = clone(snapshot)
	updated := provider.snapshots["b"]
	updated.Buckets[1].UsedPercent = 20
	provider.snapshots["b"] = updated
	service.RequestRefresh("b")
	if err = service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 0 || recoveries != 0 || len(service.Operations()) != 0 || len(service.Schedules()) != 1 {
		t.Fatalf("observation-only mode mutated reset state: writes=%d recoveries=%d operations=%d schedules=%d", len(provider.requests), recoveries, len(service.Operations()), len(service.Schedules()))
	}
	if evaluation := Evaluate(snapshot, "model", service.Settings(), now); !evaluation.Eligible || evaluation.Known || evaluation.Reason != "policy_disabled" {
		t.Fatalf("observations enabled routing policy: %+v", evaluation)
	}
}

func TestObservationOnlyRequiresDurableState(t *testing.T) {
	settings := observationSettings(t)
	settings.StateDir = ""
	_, err := NewService(Options{Settings: settings, Provider: &fakeProvider{}, Accounts: func() []Identity { return nil }})
	var policyErr *Error
	if !errors.As(err, &policyErr) || policyErr.Code != "invalid_settings" {
		t.Fatalf("observation-only mode without durable state was accepted: %v", err)
	}
}

func TestObservationEnableUpdateRequiresDurableState(t *testing.T) {
	service, err := NewService(Options{Settings: DefaultSettings(), Provider: &fakeProvider{}, Accounts: func() []Identity { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	next := service.Settings()
	next.ObservationsEnabled = true
	err = service.UpdateSettings(next)
	var policyErr *Error
	if !errors.As(err, &policyErr) || policyErr.Code != "invalid_settings" || service.Settings().ObservationsEnabled {
		t.Fatalf("runtime observation enabled without durable state: settings=%+v err=%v", service.Settings(), err)
	}
}

func TestLegacyDisabledObservationModeDoesNotCreateFiles(t *testing.T) {
	settings := DefaultSettings()
	settings.StateDir = filepath.Join(t.TempDir(), "disabled")
	reads := 0
	provider := &fakeProvider{discover: func(Identity) (Snapshot, error) { reads++; return testSnapshot("b", 7), nil }}
	service, err := NewService(Options{Settings: settings, Provider: provider, Accounts: func() []Identity { return []Identity{testSnapshot("b", 7).Identity} }})
	if err != nil {
		t.Fatal(err)
	}
	service.RequestRefresh("b")
	if err = service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.Run(ctx)
	if reads != 0 || len(service.Accounts()) != 0 {
		t.Fatalf("legacy disabled mode observed provider: %d reads", reads)
	}
	if _, err = os.Stat(settings.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy disabled mode created state files: %v", err)
	}
}

// Removing the independent observation gate must stop initial, due, and event reads.
func TestObservationOnlyTicksRefreshAndPersist(t *testing.T) {
	now := testTime()
	snapshot := testSnapshot("b", 7)
	snapshot.Credits = []Credit{testCredit("credit", nil)}
	snapshot.AvailableCredits = 1
	reads := 0
	provider := &fakeProvider{discover: func(Identity) (Snapshot, error) {
		reads++
		next := clone(snapshot)
		next.ObservedAt = now
		next.InventoryObservedAt = now
		for i := range next.Buckets {
			next.Buckets[i].ObservedAt = now
		}
		return next, nil
	}}
	opts := Options{Settings: observationSettings(t), Provider: provider, Now: func() time.Time { return now }, Accounts: func() []Identity { return []Identity{snapshot.Identity} }}
	service, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("initial observation reads = %d, want 1 while routing is off", reads)
	}
	observed, ok := service.Snapshot("b")
	if !ok || observed.Buckets[1].UsedPercent != 40 || observed.AvailableCredits != 1 || service.Settings().Enabled {
		t.Fatalf("observation-only snapshot missing or routing enabled: %+v", observed)
	}
	for i := 0; i < 3; i++ {
		if err = service.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatalf("unchanged observation polled %d times", reads)
	}
	service.RequestRefresh("b")
	service.RequestRefresh("b")
	if err = service.Tick(context.Background()); err != nil || reads != 2 {
		t.Fatalf("coalesced event did not refresh once: reads=%d err=%v", reads, err)
	}
	// Account b retains two-second jitter inside the two-minute freshness window.
	now = now.Add(time.Minute + time.Second)
	if err = service.Tick(context.Background()); err != nil || reads != 2 {
		t.Fatalf("observation ignored account polling jitter: reads=%d err=%v", reads, err)
	}
	now = now.Add(time.Second)
	if err = service.Tick(context.Background()); err != nil || reads != 3 {
		t.Fatalf("due observation did not refresh: reads=%d err=%v", reads, err)
	}
	stateInfo, err := os.Stat(filepath.Join(opts.Settings.StateDir, "state.json"))
	if err != nil || stateInfo.Mode().Perm() != 0600 {
		t.Fatalf("observation journal is missing or not private: %v", err)
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := restarted.Snapshot("b")
	if !ok || persisted.Version != 3 || !persisted.ObservedAt.Equal(now) || persisted.AvailableCredits != 1 {
		t.Fatalf("observations were not durable: %+v", persisted)
	}
}

func TestObservationOnlyExponentialBackoffIsBounded(t *testing.T) {
	now := testTime()
	reads := 0
	snapshot := testSnapshot("b", 7)
	provider := &fakeProvider{discover: func(Identity) (Snapshot, error) { reads++; return Snapshot{}, errors.New("discovery unavailable") }}
	service, err := NewService(Options{Settings: observationSettings(t), Provider: provider, Now: func() time.Time { return now }, Accounts: func() []Identity { return []Identity{snapshot.Identity} }})
	if err != nil {
		t.Fatal(err)
	}
	for attempt, second := range []int{0, 2, 6, 14, 30, 62, 126, 190} {
		if attempt > 0 {
			now = testTime().Add(time.Duration(second-1) * time.Second)
			service.RequestRefresh("b")
			if err = service.Tick(context.Background()); err != nil || reads != attempt {
				t.Fatalf("retry happened before backoff elapsed: attempt=%d reads=%d err=%v", attempt, reads, err)
			}
		}
		now = testTime().Add(time.Duration(second) * time.Second)
		if err = service.Tick(context.Background()); err == nil || reads != attempt+1 {
			t.Fatalf("retry did not follow bounded exponential backoff: attempt=%d reads=%d err=%v", attempt, reads, err)
		}
	}
}

// Startup configuration must opt out of stored collection and opt into legacy journals.
func TestObservationStartupConfigurationControlsCollection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stored     bool
		configured bool
		legacy     bool
		wantReads  int
	}{
		{name: "stored enabled config disabled", stored: true, configured: false, wantReads: 0},
		{name: "stored disabled config enabled", stored: false, configured: true, wantReads: 1},
		{name: "legacy journal config enabled", configured: true, legacy: true, wantReads: 1},
		{name: "legacy journal config disabled", configured: false, legacy: true, wantReads: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := observationSettings(t)
			settings.ObservationsEnabled = tc.stored
			reads := 0
			snapshot := testSnapshot("b", 7)
			provider := &fakeProvider{discover: func(Identity) (Snapshot, error) { reads++; return snapshot, nil }}
			opts := Options{Settings: settings, Provider: provider, Now: testTime, Accounts: func() []Identity { return []Identity{snapshot.Identity} }}
			service, err := NewService(opts)
			if err != nil {
				t.Fatal(err)
			}
			if err = service.UpdateSettings(settings); err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				path := filepath.Join(settings.StateDir, "state.json")
				data, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				var journal map[string]json.RawMessage
				var storedSettings map[string]json.RawMessage
				if err = json.Unmarshal(data, &journal); err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(journal["settings"], &storedSettings); err != nil {
					t.Fatal(err)
				}
				delete(storedSettings, "observations_enabled")
				journal["settings"], err = json.Marshal(storedSettings)
				if err != nil {
					t.Fatal(err)
				}
				data, err = json.Marshal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			opts.Settings.ObservationsEnabled = tc.configured
			restarted, err := NewService(opts)
			if err != nil {
				t.Fatal(err)
			}
			if err = restarted.Tick(context.Background()); err != nil || reads != tc.wantReads {
				t.Fatalf("startup observation reads=%d want=%d err=%v", reads, tc.wantReads, err)
			}
			if restarted.Settings().Enabled || restarted.Settings().ObservationsEnabled != tc.configured {
				t.Fatalf("startup settings do not honor observation configuration: %+v", restarted.Settings())
			}
		})
	}
}

func TestObservationRuntimeToggleStopsAndResumesCollection(t *testing.T) {
	settings := observationSettings(t)
	settings.ObservationsEnabled = false
	reads := 0
	snapshot := testSnapshot("b", 7)
	provider := &fakeProvider{discover: func(Identity) (Snapshot, error) { reads++; return snapshot, nil }}
	service, err := NewService(Options{Settings: settings, Provider: provider, Now: testTime, Accounts: func() []Identity { return []Identity{snapshot.Identity} }})
	if err != nil {
		t.Fatal(err)
	}
	settings.ObservationsEnabled = true
	if err = service.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err = service.Tick(context.Background()); err != nil || reads != 1 {
		t.Fatalf("runtime observation enable did not start collection: reads=%d err=%v", reads, err)
	}
	settings.ObservationsEnabled = false
	if err = service.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	service.RequestRefresh("b")
	if err = service.Tick(context.Background()); err != nil || reads != 1 {
		t.Fatalf("runtime observation disable continued collection: reads=%d err=%v", reads, err)
	}
	settings.ObservationsEnabled = true
	if err = service.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err = service.Tick(context.Background()); err != nil || reads != 2 {
		t.Fatalf("runtime observation resume lost queued refresh: reads=%d err=%v", reads, err)
	}
}

// The native lifecycle must retain its two-read bound and journal owner in observation mode.
func TestObservationLifecycleBoundsDiscoveryAndReleasesOwnership(t *testing.T) {
	a, b, c := testSnapshot("a", 4), testSnapshot("b", 7), testSnapshot("c", 5)
	provider := &blockingProvider{accounts: map[string]Snapshot{"a": a, "b": b, "c": c}, entered: make(chan string, 8), release: make(chan struct{}, 8)}
	settings := observationSettings(t)
	service, err := NewService(Options{Settings: settings, Provider: provider, Now: testTime, Accounts: func() []Identity { return []Identity{a.Identity, b.Identity, c.Identity} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { service.Run(ctx); close(done) }()
	awaitRead := func() string {
		t.Helper()
		select {
		case id := <-provider.entered:
			return id
		case <-time.After(5 * time.Second):
			t.Fatal("observation lifecycle did not dispatch provider reads")
			return ""
		}
	}
	first, second := awaitRead(), awaitRead()
	if first == second || provider.active.Load() != 2 {
		t.Fatal("observation lifecycle failed to run two independent account reads")
	}
	if release, errOwner := lockNamed(settings.StateDir, "owner.lock"); errOwner == nil {
		release()
		t.Fatal("observation lifecycle did not retain journal ownership")
	}
	provider.release <- struct{}{}
	third := awaitRead()
	if third == first || third == second {
		t.Fatal("observation lifecycle duplicated an in-flight account read")
	}
	provider.release <- struct{}{}
	provider.release <- struct{}{}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observation lifecycle did not finish after cancellation")
	}
	if provider.calls.Load() != 3 || provider.maximum.Load() != 2 || len(service.Accounts()) != 3 || service.Settings().Enabled {
		t.Fatalf("observation lifecycle changed polling or routing: reads=%d concurrency=%d snapshots=%d", provider.calls.Load(), provider.maximum.Load(), len(service.Accounts()))
	}
	release, err := lockNamed(settings.StateDir, "owner.lock")
	if err != nil {
		t.Fatalf("observation lifecycle retained ownership after stopping: %v", err)
	}
	release()
}

func TestObservationReplicaCannotOverwriteOwnerJournal(t *testing.T) {
	settings := observationSettings(t)
	snapshot := testSnapshot("b", 7)
	provider := &fakeProvider{snapshots: map[string]Snapshot{"b": snapshot}}
	service, err := NewService(Options{Settings: settings, Provider: provider, Now: testTime, Accounts: func() []Identity { return []Identity{snapshot.Identity} }})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(settings.StateDir, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockNamed(settings.StateDir, "owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.Run(ctx)
	if !service.Settings().ReadOnly {
		t.Fatal("observation replica retained reset authority")
	}
	if err = service.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("observation replica overwrote the owner's durable journal")
	}
	settings = service.Settings()
	settings.ReadOnly = false
	if err = service.UpdateSettings(settings); err == nil {
		t.Fatal("observation replica reclaimed write authority")
	}
}
