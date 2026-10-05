package accountpolicy

import (
	"context"
	"testing"
	"time"
)

// Idle accounts must be reread before healthy evidence becomes stale, without
// tool-request polling or bypassing provider backoff.
func TestObservationCadenceRefreshesWithinFreshnessWindow(t *testing.T) {
	for _, freshness := range []int{120, 8, 1} {
		t.Run((time.Duration(freshness) * time.Second).String(), func(t *testing.T) {
			now := testTime()
			snapshot := testSnapshot("b", 7)
			reads := 0
			provider := &fakeProvider{discover: func(Identity) (Snapshot, error) {
				reads++
				next := clone(snapshot)
				next.ObservedAt, next.InventoryObservedAt = now, now
				for i := range next.Buckets {
					next.Buckets[i].ObservedAt = now
				}
				return next, nil
			}}
			settings := observationSettings(t)
			settings.FreshnessSeconds = freshness
			service, err := NewService(Options{Settings: settings, Provider: provider, Now: func() time.Time { return now }, Accounts: func() []Identity { return []Identity{snapshot.Identity} }})
			if err != nil {
				t.Fatal(err)
			}
			if err = service.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Duration(freshness) * time.Second)
			if err = service.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			observed, _ := service.Snapshot("b")
			if reads != 2 || !observed.ObservedAt.Equal(now) {
				t.Fatalf("healthy idle quota was allowed to expire: reads=%d observed=%v now=%v", reads, observed.ObservedAt, now)
			}
		})
	}
}
