package accountpolicy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeProvider struct {
	mu        sync.Mutex
	snapshots map[string]Snapshot
	consume   func(Identity, string, string) (ConsumeResult, error)
	discover  func(Identity) (Snapshot, error)
	requests  []string
	credits   []string
}

func (p *fakeProvider) Discover(_ context.Context, id Identity) (Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discover != nil {
		return p.discover(id)
	}
	return p.snapshots[id.CredentialID], nil
}
func (p *fakeProvider) Consume(_ context.Context, id Identity, request, credit string) (ConsumeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	p.credits = append(p.credits, credit)
	if p.consume != nil {
		return p.consume(id, request, credit)
	}
	return ConsumeResult{Code: "nothing_to_reset"}, nil
}
func fixtureService(t *testing.T, p *fakeProvider, now *time.Time, settings Settings) (*Service, Options) {
	t.Helper()
	if settings.StateDir == "" {
		settings.StateDir = t.TempDir()
	}
	opts := Options{Settings: settings, Provider: p, Now: func() time.Time { return *now }, Accounts: func() []Identity {
		p.mu.Lock()
		defer p.mu.Unlock()
		var ids []Identity
		for _, s := range p.snapshots {
			ids = append(ids, s.Identity)
		}
		return ids
	}, HasDemand: func(string) bool { return true }, IsIdle: func(string) bool { return true }}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	return s, opts
}

func TestRedemptionDurableLostResponseAndRestart(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("first", &expiry), testCredit("second", nil)}
	b.AvailableCredits = 2
	b.Buckets[1].UsedPercent = 100
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	s, opts := fixtureService(t, p, &now, settings)
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		return ConsumeResult{}, errors.New("lost response with SECRET_TOKEN")
	}
	op, err := s.Redeem(context.Background(), "b", "first")
	if err == nil || op.State != "outcome_unknown" || op.RequestID == "" {
		t.Fatalf("uncertain result %+v %v", op, err)
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		next := p.snapshots["b"]
		next.Credits = next.Credits[1:]
		next.AvailableCredits = 1
		next.Buckets[1].UsedPercent = 15
		next.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
		next.ObservedAt = now
		next.Buckets[1].ObservedAt = now
		p.snapshots["b"] = next
		return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
	}
	retried, err := restarted.Redeem(context.Background(), "b", "first")
	if err != nil || retried.State != "confirmed" || retried.RequestID != op.RequestID {
		t.Fatalf("durable retry %+v %v", retried, err)
	}
	if len(p.requests) != 2 || p.requests[0] != p.requests[1] || p.credits[0] != "first" || p.credits[1] != "first" {
		t.Fatalf("non-idempotent selected write: %v %v", p.requests, p.credits)
	}
	if _, err = restarted.Redeem(context.Background(), "b", "second"); err == nil {
		t.Fatal("spent another credit before renewed depletion")
	}
	if retried.Error != "" {
		t.Fatal("confirmed retained provider error")
	}
}

func TestSchedulerTwoAccountScenarioAndCreditOrdering(t *testing.T) {
	now := testTime()
	a, b := testSnapshot("a", 4), testSnapshot("b", 7)
	first := now.Add(24 * time.Hour)
	second := now.Add(18 * 24 * time.Hour)
	third := now.Add(25 * 24 * time.Hour)
	b.Credits = []Credit{testCredit("third", &third), testCredit("first", &first), testCredit("second", &second)}
	b.AvailableCredits = 3
	p := &fakeProvider{snapshots: map[string]Snapshot{"a": a, "b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	s, _ := fixtureService(t, p, &now, settings)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 0 {
		t.Fatal("redeemed before guard without exhaustion")
	}
	now = first.Add(-10 * time.Minute)
	p.mu.Lock()
	for id, snap := range p.snapshots {
		snap.ObservedAt = now
		snap.InventoryObservedAt = now
		for i := range snap.Buckets {
			snap.Buckets[i].ObservedAt = now
		}
		p.snapshots[id] = snap
	}
	p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
		if id.CredentialID != "b" || credit != "first" {
			return ConsumeResult{}, errors.New("wrong ownership or credit")
		}
		next := p.snapshots["b"]
		next.Credits = []Credit{next.Credits[0], next.Credits[2]}
		next.AvailableCredits = 2
		next.Buckets[1].UsedPercent = 5
		next.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
		p.snapshots["b"] = next
		return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
	}
	p.mu.Unlock()
	if err := s.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := s.Operations()
	if len(ops) != 1 || ops[0].State != "confirmed" || ops[0].CreditID != "first" {
		t.Fatalf("deadline reset: %+v", ops)
	}
	updated, _ := s.Snapshot("b")
	if len(updated.Credits) != 2 {
		t.Fatal("remaining credits lost")
	}
	settings.Automation = "off"
	if !Evaluate(a, "model", settings, testTime()).Deadline.Before(Evaluate(updated, "model", settings, now).Deadline) {
		t.Fatal("normal priority not recalculated")
	}
}

func TestSettingsSchedulesAndSnapshotPersistence(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("credit", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	initial := DefaultSettings()
	initial.Enabled = true
	initial.Automation = "notify"
	s, opts := fixtureService(t, p, &now, initial)
	sch, err := s.Schedule("b", "credit", now.Add(20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Schedule("b", "credit", expiry); err == nil {
		t.Fatal("schedule after expiry accepted")
	}
	settings := s.Settings()
	settings.Automation = "notify"
	settings.Accounts["b"] = AccountControl{Hold: true}
	if err = s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Settings().Automation != "notify" || !restarted.Settings().Accounts["b"].Hold || len(restarted.Schedules()) != 1 || restarted.Schedules()[0].ID != sch.ID {
		t.Fatal("settings/schedule not durable")
	}
	clone, _ := restarted.Snapshot("b")
	clone.Credits[0].ID = "tampered"
	original, _ := restarted.Snapshot("b")
	if original.Credits[0].ID != "credit" {
		t.Fatal("snapshot alias leaked")
	}
	if err = restarted.CancelSchedule(sch.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err = NewService(opts)
	if err != nil || len(restarted.Schedules()) != 0 {
		t.Fatal("cancel not durable")
	}
}

func TestReadOnlyIdentityAndDistinctOutcomes(t *testing.T) {
	for _, code := range []string{"nothing_to_reset", "no_credit", "already_redeemed"} {
		t.Run(code, func(t *testing.T) {
			now := testTime()
			b := testSnapshot("b", 7)
			expiry := now.Add(time.Hour)
			b.Credits = []Credit{testCredit("credit", &expiry)}
			b.AvailableCredits = 1
			p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}, consume: func(Identity, string, string) (ConsumeResult, error) { return ConsumeResult{Code: code}, nil }}
			settings := DefaultSettings()
			settings.Enabled = true
			s, _ := fixtureService(t, p, &now, settings)
			op, _ := s.Redeem(context.Background(), "b", "credit")
			want := code
			if code == "already_redeemed" {
				want = "outcome_unknown"
			}
			if op.State != want || op.Result != code {
				t.Fatalf("outcome %+v", op)
			}
		})
	}
	now := testTime()
	b := testSnapshot("b", 7)
	expiry := now.Add(time.Hour)
	b.Credits = []Credit{testCredit("credit", &expiry)}
	b.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.ReadOnly = true
	s, _ := fixtureService(t, p, &now, settings)
	if _, err := s.Redeem(context.Background(), "b", "credit"); err == nil || len(p.requests) != 0 {
		t.Fatal("read-only wrote")
	}
	settings = DefaultSettings()
	settings.Enabled = true
	s, _ = fixtureService(t, p, &now, settings)
	p.discover = func(id Identity) (Snapshot, error) { wrong := b; wrong.Identity.AccountID = "other"; return wrong, nil }
	if err := s.Refresh(context.Background(), "b"); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	if _, err := s.Redeem(context.Background(), "b", "credit"); err == nil || len(p.requests) != 0 {
		t.Fatal("identity mismatch wrote")
	}
}
