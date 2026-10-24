package accountpolicy

import (
	"context"
	"testing"
	"time"
)

func TestPressureOverrideSelectionSafetyAndCancellation(t *testing.T) {
	for _, reason := range []string{"cancelled", "expired", "removed", "reassigned", "quota", "signal"} {
		t.Run(reason, func(t *testing.T) {
			s, _, p, now := splitFixture(t)
			active, err := s.ReplacePressureOverride([]RoutingShare{{CredentialID: "a"}, {CredentialID: "b"}}, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			if id, members := s.RoutingPressure("codex"); id != active.ID || len(members) != 2 {
				t.Fatal("missing pressure authority")
			}
			if s.PreferredAccount("codex") != "" {
				t.Fatal("pressure became single-account override")
			}
			if id, _ := s.RoutingSplit("codex"); id != "" {
				t.Fatal("pressure became a percentage split")
			}
			switch reason {
			case "cancelled":
				if _, err := s.CancelOverride(active.Revision); err != nil {
					t.Fatal(err)
				}
			case "expired":
				*now = now.Add(time.Hour)
			case "removed":
				delete(p.snapshots, "b")
			case "reassigned":
				b := p.snapshots["b"]
				b.Identity.AccountID = "different-owner"
				p.snapshots["b"] = b
			case "quota":
				b := clone(p.snapshots["b"])
				b.Buckets[0].UsedPercent = 100
				p.snapshots["b"] = b
				if err := s.Refresh(context.Background(), "b"); err != nil {
					t.Fatal(err)
				}
			case "signal":
				if err := s.ConfirmOverrideExhaustion(p.snapshots["b"].Identity); err != nil {
					t.Fatal(err)
				}
			}
			if id, _ := s.RoutingPressure("codex"); id != "" {
				t.Fatalf("unsafe authority survived %s", reason)
			}
		})
	}
}

func TestPressureOverrideRejectsDuplicateLogicalAccountBeforePersistence(t *testing.T) {
	s, opts, p, _ := splitFixture(t)
	active, err := s.ReplaceOverride("a", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	original := clone(p.snapshots["b"])
	b := clone(original)
	b.Identity.AccountID = p.snapshots["a"].Identity.AccountID
	b.Identity.WorkspaceID = p.snapshots["a"].Identity.WorkspaceID
	p.snapshots["b"] = b
	if err := s.Refresh(context.Background(), "b"); err == nil {
		t.Fatal("duplicate ownership refresh accepted")
	}
	if _, err := s.ReplacePressureOverride([]RoutingShare{{CredentialID: "a"}, {CredentialID: "b"}}, 1, active.Revision); err == nil {
		t.Fatal("two credentials for one account replaced valid override")
	}
	if got := s.RoutingOverride(); got.ID != active.ID || got.Revision != active.Revision {
		t.Fatal("rejected override mutated journal")
	}
	p.snapshots["b"] = original
	if _, err := NewService(opts); err != nil {
		t.Fatalf("rejected pressure corrupted restart: %v", err)
	}
}
