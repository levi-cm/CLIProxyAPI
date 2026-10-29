package accountpolicy

import (
	"context"
	"testing"
	"time"
)

func TestManualAndAutomaticResetDuringActiveInference(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[automatic], func(t *testing.T) {
			now := testTime()
			before := testSnapshot("busy", 7)
			expiry := now.Add(9 * time.Minute)
			before.Credits = []Credit{testCredit("expiring", &expiry)}
			before.AvailableCredits = 1
			before.ActiveRequests = 3
			before.ActiveBindings = 2
			p := &fakeProvider{snapshots: map[string]Snapshot{"busy": before}}
			settings := DefaultSettings()
			settings.Enabled = true
			settings.Automation = "auto_expiring"
			_, opts := fixtureService(t, p, &now, settings)
			opts.IsIdle = func(string) bool { return false }
			held, released := false, false
			opts.AcquireReset = func(context.Context, string) (func(), error) {
				held = true
				return func() { released = true; held = false }, nil
			}
			p.consume = func(id Identity, request, credit string) (ConsumeResult, error) {
				if !held || id != before.Identity || credit != "expiring" {
					t.Fatal("reset lost serialization or identity")
				}
				after := p.snapshots[id.CredentialID]
				after.Buckets[1].UsedPercent = 0
				after.Buckets[1].ResetAt = now.Add(8 * 24 * time.Hour)
				after.Credits = nil
				after.AvailableCredits = 0
				p.snapshots[id.CredentialID] = after
				return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
			}
			s, err := NewService(opts)
			if err != nil {
				t.Fatal(err)
			}
			if automatic {
				err = s.Tick(context.Background())
			} else {
				_, err = s.Redeem(context.Background(), "busy", "expiring")
			}
			if err != nil {
				t.Fatalf("active traffic blocked reset: %v", err)
			}
			if len(p.requests) != 1 || len(s.Operations()) != 1 || s.Operations()[0].State != "confirmed" || !released {
				t.Fatalf("unconfirmed/nonserialized reset: operations=%+v calls=%d released=%v", s.Operations(), len(p.requests), released)
			}
			after, _ := s.Snapshot("busy")
			if after.ActiveRequests != 3 || after.ActiveBindings != 2 {
				t.Fatal("reset changed active inference bookkeeping")
			}
		})
	}
}
