package accountpolicy

import (
	"context"
	"testing"
	"time"
)

func TestHistoryObservationsOutsideLockAndHonestRefills(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.ObservationsEnabled = true
	settings.UsageHistoryEnabled = true
	s, _ := fixtureService(t, p, &now, settings)
	var obs []Snapshot
	s.SetObservationObserver(func(old, next Snapshot, ops []Operation) { _ = s.Settings(); obs = append(obs, next) })
	var err error
	if err = s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	b.ObservedAt = now
	for i := range b.Buckets {
		b.Buckets[i].ObservedAt = now
		b.Buckets[i].UsedPercent = 0
	}
	p.snapshots["b"] = b
	if err = s.Refresh(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 || obs[1].Buckets[0].UsedPercent != 0 {
		t.Fatalf("observations %+v", obs)
	}
}

func TestConfirmedOperationNotifiesArchiveOutsideLock(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.UsageHistoryEnabled = true
	s, _ := fixtureService(t, p, &now, settings)
	confirmed := 0
	s.SetObservationObserver(func(old, next Snapshot, ops []Operation) {
		_ = s.Settings()
		for _, op := range ops {
			if op.State == "confirmed" {
				confirmed++
			}
		}
	})
	if err := s.saveOperation(Operation{ID: "confirmed", CredentialID: "b", State: "confirmed", After: &b}); err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 {
		t.Fatalf("manual reset events %d", confirmed)
	}
}

func TestHistoryCollectionTransitionsNotifyAfterPersistenceOutsideLock(t *testing.T) {
	now := testTime()
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": testSnapshot("b", 7)}}
	s, _ := fixtureService(t, p, &now, DefaultSettings())
	var observed []bool
	s.SetHistoryCollectionObserver(func(enabled bool) {
		if s.Settings().UsageHistoryEnabled != enabled {
			t.Fatal("uncommitted collection setting")
		}
		observed = append(observed, enabled)
	})
	for _, enabled := range []bool{true, true, false} {
		if err := s.MergeSettings(func(current Settings) (Settings, error) { current.UsageHistoryEnabled = enabled; return current, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(observed) != 2 || !observed[0] || observed[1] {
		t.Fatalf("collection transitions: %v", observed)
	}
}
