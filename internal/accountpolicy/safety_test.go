package accountpolicy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentRedemptionDoesNotSpendTwoCredits(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry), testCredit("second", &expiry)}
	b.AvailableCredits = 2
	b.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		next := clone(p.snapshots[id.CredentialID])
		next.Credits = []Credit{testCredit("second", &expiry)}
		next.AvailableCredits = 1
		next.Buckets[1].UsedPercent = 10
		next.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
		p.snapshots[id.CredentialID] = next
		return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
	}
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Redeem(context.Background(), "b", "first") }()
	}
	wg.Wait()
	if len(p.requests) != 1 || len(s.Operations()) != 1 || s.Operations()[0].State != "confirmed" {
		t.Fatalf("concurrent operation duplicated: %v %+v", p.requests, s.Operations())
	}
}

func TestLostResponseReconcilesWithoutSecondWrite(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	b.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, opts := fixtureService(t, p, &now, settings)
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		next := clone(p.snapshots["b"])
		next.Credits = nil
		next.AvailableCredits = 0
		next.Buckets[1].UsedPercent = 7
		next.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
		p.snapshots["b"] = next
		return ConsumeResult{}, errors.New("lost response")
	}
	first, err := s.Redeem(context.Background(), "b", "first")
	if err == nil || first.State != "outcome_unknown" {
		t.Fatal("lost response not durable")
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Redeem(context.Background(), "b", "first")
	if err != nil || recovered.State != "confirmed" || len(p.requests) != 1 {
		t.Fatalf("did not reconcile recovered response %+v %v", recovered, err)
	}
}

type blockingProvider struct {
	accounts map[string]Snapshot
	entered  chan string
	release  chan struct{}
	active   atomic.Int32
	maximum  atomic.Int32
	calls    atomic.Int32
}

func (p *blockingProvider) Discover(ctx context.Context, id Identity) (Snapshot, error) {
	p.calls.Add(1)
	active := p.active.Add(1)
	for old := p.maximum.Load(); active > old; old = p.maximum.Load() {
		if p.maximum.CompareAndSwap(old, active) {
			break
		}
	}
	p.entered <- id.CredentialID
	select {
	case <-p.release:
	case <-ctx.Done():
		p.active.Add(-1)
		return Snapshot{}, ctx.Err()
	}
	p.active.Add(-1)
	return p.accounts[id.CredentialID], nil
}
func (*blockingProvider) Consume(context.Context, Identity, string, string) (ConsumeResult, error) {
	return ConsumeResult{}, nil
}

func TestDiscoveryConcurrencyBoundAndPerAccountSerialization(t *testing.T) {
	now := testTime()
	a, b, c := testSnapshot("a", 4), testSnapshot("b", 7), testSnapshot("c", 5)
	p := &blockingProvider{accounts: map[string]Snapshot{"a": a, "b": b, "c": c}, entered: make(chan string, 8), release: make(chan struct{}, 8)}
	settings := DefaultSettings()
	settings.StateDir = t.TempDir()
	s, err := NewService(Options{Settings: settings, Provider: p, Accounts: func() []Identity { return []Identity{a.Identity, b.Identity, c.Identity} }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Refresh(context.Background(), "") }()
	first, second := <-p.entered, <-p.entered
	if first == second {
		t.Fatal("one account discovered concurrently")
	}
	if p.active.Load() != 2 {
		t.Fatal("independent reads failed to run concurrently")
	}
	p.release <- struct{}{}
	third := <-p.entered
	if third == first || third == second {
		t.Fatal("repeated account instead of queued third")
	}
	p.release <- struct{}{}
	p.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.maximum.Load() > 2 {
		t.Fatal("pool concurrency exceeded")
	}
	go func() { done <- s.Refresh(context.Background(), "a") }()
	<-p.entered
	queued := make(chan struct{})
	go func() { close(queued); done <- s.Refresh(context.Background(), "a") }()
	<-queued
	if p.active.Load() != 1 {
		t.Fatal("same account discovered concurrently")
	}
	p.release <- struct{}{}
	<-p.entered
	p.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestJournalBeforeWriteAndSanitizedFailures(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		data, err := os.ReadFile(filepath.Join(s.Settings().StateDir, "state.json"))
		if err != nil {
			return ConsumeResult{}, err
		}
		if !strings.Contains(string(data), request) || !strings.Contains(string(data), `"state":"submitted"`) {
			return ConsumeResult{}, errors.New("not durable before transport")
		}
		return ConsumeResult{}, errors.New("SECRET_TOKEN prompt body")
	}
	op, _ := s.Redeem(context.Background(), "b", "first")
	if op.State != "outcome_unknown" {
		t.Fatal("failed write not journaled")
	}
	data, err := os.ReadFile(filepath.Join(s.Settings().StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET_TOKEN") || strings.Contains(string(data), "prompt body") {
		t.Fatal("secret persisted")
	}
	info, err := os.Stat(filepath.Join(s.Settings().StateDir, "state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe journal permissions")
	}
}

func TestDisabledTicksAndConfigRollbackDoNotWrite(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	calls := 0
	p.discover = func(Identity) (Snapshot, error) { calls++; return b, nil }
	settings := DefaultSettings()
	settings.StateDir = t.TempDir()
	opts := Options{Settings: settings, Provider: p, Accounts: func() []Identity { return []Identity{b.Identity} }, Now: func() time.Time { return now }}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(context.Background()); err != nil || calls != 0 {
		t.Fatal("disabled module polled")
	}
	enabled := s.Settings()
	enabled.Enabled = true
	enabled.Automation = "auto_expiring"
	if err = s.UpdateSettings(enabled); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Settings().Enabled || restarted.Settings().Automation != "off" {
		t.Fatal("config rollback did not override persisted authority")
	}
}

func TestCooldownRecoveryOnlyWithNewerConfirmedPermission(t *testing.T) {
	now := testTime()
	before := testSnapshot("b", 7)
	before.Buckets[1].UsedPercent = 100
	denied := false
	before.Buckets[1].Allowed = &denied
	after := clone(before)
	after.Buckets[1].UsedPercent = 0
	if recovered(before, after) {
		t.Fatal("low percentage overrode denied permission")
	}
	allowed := true
	after.Buckets[1].Allowed = &allowed
	after.Buckets[1].ObservedAt = now.Add(-time.Second)
	if recovered(before, after) {
		t.Fatal("older evidence cleared newer failure")
	}
	after.Buckets[1].ObservedAt = now
	after.Identity.Generation = 2
	if recovered(before, after) {
		t.Fatal("changed credentials cleared old failure")
	}
}

func TestBusyLeasePreventsJournalPreparation(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, opts := fixtureService(t, p, &now, settings)
	opts.AcquireReset = func(context.Context, string) (func(), error) { return nil, errors.New("busy") }
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Redeem(context.Background(), "b", "first"); err == nil || len(s.Operations()) != 0 || len(p.requests) != 0 {
		t.Fatal("active inference reset lease ignored")
	}
}

func TestNothingToResetDoesNotRepollEveryTick(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(9 * time.Minute)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	s, _ := fixtureService(t, p, &now, settings)
	calls := 0
	p.discover = func(Identity) (Snapshot, error) { calls++; return b, nil }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstCalls := calls
	for n := 0; n < 4; n++ {
		now = now.Add(time.Second)
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != firstCalls || len(p.requests) != 1 {
		t.Fatalf("unchanged evidence repolled or spent: reads=%d initial=%d writes=%d", calls, firstCalls, len(p.requests))
	}
}

func TestFailedCooldownPersistenceRetriesAcrossRestart(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	b.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, opts := fixtureService(t, p, &now, settings)
	attempts := 0
	opts.Recover = func(context.Context, Snapshot, Snapshot) error {
		attempts++
		if attempts == 1 {
			return errors.New("persistence unavailable")
		}
		return nil
	}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	next := clone(b)
	next.Buckets[1].UsedPercent = 5
	next.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
	p.snapshots["b"] = next
	if err = s.Refresh(context.Background(), "b"); err == nil {
		t.Fatal("failed recovery hidden")
	}
	s, err = NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal("durable failed cooldown recovery was forgotten")
	}
}

func TestExpiredScheduleRetainsOperationHistory(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	if _, err := s.Schedule("b", "first", now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	b.ObservedAt = now
	b.InventoryObservedAt = now
	for i := range b.Buckets {
		b.Buckets[i].ObservedAt = now
	}
	p.snapshots["b"] = b
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.Schedules()) != 0 || len(s.Operations()) != 1 || s.Operations()[0].State != "expired" || len(p.requests) != 0 {
		t.Fatal("expired scheduled reset not retained as history")
	}
}

func TestRecoveryGenerationCapturedAtDiscoveryStart(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	b.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	_, opts := fixtureService(t, p, &now, settings)
	identity := b.Identity
	identity.Generation = 2
	opts.Accounts = func() []Identity { return []Identity{identity} }
	recoveredGeneration := uint64(0)
	opts.Recover = func(_ context.Context, before, after Snapshot) error {
		recoveredGeneration = before.Identity.Generation
		return nil
	}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	after := clone(b)
	after.Identity = identity
	after.Buckets[1].UsedPercent = 5
	after.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
	p.snapshots["b"] = after
	if err = s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if recoveredGeneration != 2 {
		t.Fatal("auth generation change before discovery prevented quota recovery")
	}
	identity.Generation = 3
	recoveredGeneration = 0
	before := clone(after)
	before.Identity = identity
	before.Buckets[1].UsedPercent = 100
	p.snapshots["b"] = before
	if err = s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	p.discover = func(id Identity) (Snapshot, error) { identity.Generation = 4; after.Identity = id; return after, nil }
	_ = s.Refresh(context.Background(), "b")
	if recoveredGeneration != 0 {
		t.Fatal("newer failure during read cleared")
	}
}

func TestConfirmedResetUsesObservedQuotaEvenWhenDeadlineUnchanged(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		after := clone(b)
		after.Credits = nil
		after.AvailableCredits = 0
		after.Buckets[1].UsedPercent = 10
		p.snapshots["b"] = after
		return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
	}
	op, err := s.Redeem(context.Background(), "b", "first")
	if err != nil || op.State != "confirmed" || !op.After.Buckets[1].ResetAt.Equal(b.Buckets[1].ResetAt) {
		t.Fatalf("invented deadline required to verify actual quota recovery: %+v %v", op, err)
	}
}

type delayedError struct{}

func (delayedError) Error() string             { return "rate limited" }
func (delayedError) RetryDelay() time.Duration { return 5 * time.Minute }

func TestDiscoveryHonorsRateLimitBackoffAndKeepsStaleVisible(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	reads := 0
	p.discover = func(Identity) (Snapshot, error) { reads++; return Snapshot{}, delayedError{} }
	_ = s.Refresh(context.Background(), "b")
	snapshot, _ := s.Snapshot("b")
	if snapshot.LastError == "" || !snapshot.ObservedAt.Equal(testTime()) {
		t.Fatal("read failure hid stale evidence")
	}
	now = now.Add(4 * time.Minute)
	_ = s.Tick(context.Background())
	if reads != 1 {
		t.Fatal("rate limit backoff ignored")
	}
	now = now.Add(time.Minute)
	_ = s.Tick(context.Background())
	if reads != 2 {
		t.Fatal("refresh not resumed at backoff boundary")
	}
}

func TestSnapshotInstantsNormalizedToUTC(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	loc := time.FixedZone("GMT+2", 7200)
	expiry := time.Date(2026, 10, 29, 19, 48, 0, 0, loc)
	b.Credits = []Credit{testCredit("credit", &expiry)}
	b.Credits[0].GrantedAt = now.In(loc)
	b.Buckets[1].ResetAt = b.Buckets[1].ResetAt.In(loc)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	s, _ := fixtureService(t, p, &now, DefaultSettings())
	got, _ := s.Snapshot("b")
	if got.Credits[0].ExpiresAt.Location() != time.UTC || !got.Credits[0].ExpiresAt.Equal(time.Date(2026, 10, 29, 17, 48, 0, 0, time.UTC)) || got.Buckets[1].ResetAt.Location() != time.UTC {
		t.Fatal("display offset reinterpreted instant or persisted outside UTC")
	}
}

func TestExpiringCreditExhaustionBeforeNaturalResetNeedsNoDemand(t *testing.T) {
	for _, tc := range []struct {
		name          string
		expiry        time.Duration
		weekly, short float64
		want          int
	}{{"expires before natural", 12 * time.Hour, 100, 20, 1}, {"expires after natural", 18 * 24 * time.Hour, 100, 20, 0}, {"expiry equals natural", 4 * 24 * time.Hour, 100, 20, 0}, {"short exhausted only", 12 * time.Hour, 40, 100, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			now := testTime()
			a := testSnapshot("a", 4)
			expiry := now.Add(tc.expiry)
			a.Credits = []Credit{testCredit("first", &expiry)}
			a.AvailableCredits = 1
			a.Buckets[1].UsedPercent = tc.weekly
			a.Buckets[0].UsedPercent = tc.short
			p := &fakeProvider{snapshots: map[string]Snapshot{"a": a}}
			settings := DefaultSettings()
			settings.Enabled = true
			settings.Automation = "auto_expiring"
			_, opts := fixtureService(t, p, &now, settings)
			opts.HasDemand = nil
			opts.Recover = func(context.Context, Snapshot, Snapshot) error { return nil }
			s, err := NewService(opts)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			a.ObservedAt = now
			a.InventoryObservedAt = now
			for i := range a.Buckets {
				a.Buckets[i].ObservedAt = now
			}
			p.snapshots["a"] = a
			p.consume = func(Identity, string, string) (ConsumeResult, error) {
				after := clone(a)
				after.Credits = nil
				after.AvailableCredits = 0
				after.Buckets[1].UsedPercent = 10
				after.Buckets[1].ResetAt = now.Add(7 * 24 * time.Hour)
				p.snapshots["a"] = after
				return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
			}
			if err = s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(p.requests) != tc.want {
				t.Fatalf("redemptions=%d want=%d", len(p.requests), tc.want)
			}
			if tc.want == 1 && s.Operations()[0].State != "confirmed" {
				t.Fatal("weekly reset was not verified")
			}
		})
	}
}

func TestCredentialChangeRefreshesBeforeIdleCadence(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	reads := 0
	p.discover = func(id Identity) (Snapshot, error) {
		reads++
		next := p.snapshots["b"]
		next.Identity = id
		return next, nil
	}
	next := p.snapshots["b"]
	next.Identity.Generation = 2
	p.snapshots["b"] = next
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatal("changed credentials waited for idle discovery cadence")
	}
}

func TestSecondLifecycleOwnerIsReadOnly(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	release, err := lockNamed(s.Settings().StateDir, "owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if !s.Settings().ReadOnly {
		t.Fatal("second lifecycle owner acquired reset authority")
	}
	settings = s.Settings()
	settings.ReadOnly = false
	if err = s.UpdateSettings(settings); err == nil {
		t.Fatal("replica reclaimed reset authority through management patch")
	}
}

func TestObservationReconcilesPendingOperationWithoutAutomaticWrites(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry)}
	b.AvailableCredits = 1
	b.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "off"
	s, opts := fixtureService(t, p, &now, settings)
	p.consume = func(Identity, string, string) (ConsumeResult, error) {
		after := clone(b)
		after.Credits = nil
		after.AvailableCredits = 0
		after.Buckets[1].UsedPercent = 15
		after.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
		p.snapshots["b"] = after
		return ConsumeResult{}, errors.New("lost response")
	}
	first, _ := s.Redeem(context.Background(), "b", "first")
	if first.State != "outcome_unknown" {
		t.Fatal("expected unknown outcome")
	}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if s.Operations()[0].State != "confirmed" || len(p.requests) != 1 {
		t.Fatal("read-only evidence failed to reconcile saved unknown operation")
	}
}
