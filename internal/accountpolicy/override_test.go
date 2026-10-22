package accountpolicy

import (
	"context"
	"testing"
	"time"
)

func TestOverrideRevisionLifecycleAndRestart(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.ForceAccount = "legacy"
	s, opts := fixtureService(t, p, &now, settings)
	for _, hours := range []int{0, 25, -1} {
		if _, err := s.ReplaceOverride("b", hours, 0); err == nil {
			t.Fatalf("accepted %d", hours)
		}
	}
	o, err := s.ReplaceOverride("b", 24, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Settings().ForceAccount != "" || o.State != "active" {
		t.Fatalf("override %+v", o)
	}
	if _, err = s.CancelOverride(0); err == nil {
		t.Fatal("accepted stale cancel")
	}
	s, err = NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.PreferredAccount("codex") != "b" {
		t.Fatal("restart lost preference")
	}
	now = now.Add(24 * time.Hour)
	if s.PreferredAccount("codex") != "" {
		t.Fatal("expired selection effective")
	}
	if err = s.MaintainOverride(); err != nil {
		t.Fatal(err)
	}
	if s.RoutingOverride().State != "expired" {
		t.Fatal("expiry not persisted")
	}
}
func TestOverrideExhaustionRemovalAndPolicyDisable(t *testing.T) {
	for _, reason := range []string{"exhausted", "unavailable", "policy-disabled"} {
		t.Run(reason, func(t *testing.T) {
			now := testTime()
			b := testSnapshot("b", 7)
			p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
			settings := DefaultSettings()
			settings.Enabled = true
			s, _ := fixtureService(t, p, &now, settings)
			o, err := s.ReplaceOverride("b", 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.MergeSettings(func(v Settings) (Settings, error) { v.ForceAccount = "other"; return v, nil }); err == nil {
				t.Fatal("accepted conflicting force")
			}
			switch reason {
			case "exhausted":
				b.Buckets[0].UsedPercent = 100
				p.snapshots["b"] = b
				if err = s.Refresh(context.Background(), "b"); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				delete(p.snapshots, "b")
			case "policy-disabled":
				v := s.Settings()
				v.Enabled = false
				if err = s.UpdateSettings(v); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.MaintainOverride(); err != nil {
				t.Fatal(err)
			}
			o = s.RoutingOverride()
			if o.State != reason {
				t.Fatalf("want %s got %+v", reason, o)
			}
			if s.PreferredAccount("codex") != "" {
				t.Fatal("terminated preference effective")
			}
		})
	}
}

func TestTransientAuthenticationDoesNotTerminateOverride(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	if _, err := s.ReplaceOverride("b", 1, 0); err != nil {
		t.Fatal(err)
	}
	s.opts.OverrideAccountState = func(string) (Identity, string) { return b.Identity, "temporary" }
	delete(p.snapshots, "b") // runtime's eligible-account list omits refreshing credentials
	if err := s.MaintainOverride(); err != nil {
		t.Fatal(err)
	}
	if s.RoutingOverride().State != "active" {
		t.Fatal("transient refresh ended override")
	}
	s.opts.OverrideAccountState = func(string) (Identity, string) { return b.Identity, "removed" }
	if err := s.MaintainOverride(); err != nil {
		t.Fatal(err)
	}
	if s.RoutingOverride().State != "unavailable" {
		t.Fatal("removed account kept override")
	}
}

func TestOverrideReplacementCancellationAndIdentityReassignment(t *testing.T) {
	now := testTime()
	b := testSnapshot("b", 7)
	p := &fakeProvider{snapshots: map[string]Snapshot{"b": b}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, opts := fixtureService(t, p, &now, settings)
	first, err := s.ReplaceOverride("b", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReplaceOverride("b", 24, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.Revision <= first.Revision {
		t.Fatal("replacement did not create authoritative revision")
	}
	cancelled, err := s.CancelOverride(second.Revision)
	if err != nil || cancelled.State != "cancelled" || s.PreferredAccount("codex") != "" {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	active, err := s.ReplaceOverride("b", 1, cancelled.Revision)
	if err != nil {
		t.Fatal(err)
	}
	b.Identity.AccountID = "reassigned"
	p.snapshots["b"] = b
	if s.PreferredAccount("codex") != "" {
		t.Fatal("reassigned account inherited override")
	}
	if err = s.MaintainOverride(); err != nil {
		t.Fatal(err)
	}
	if s.RoutingOverride().State != "unavailable" {
		t.Fatal("identity reassignment not persisted")
	}
	restart, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restart.RoutingOverride().Revision <= active.Revision || restart.RoutingOverride().State != "unavailable" {
		t.Fatal("terminated state lost on restart")
	}
}
