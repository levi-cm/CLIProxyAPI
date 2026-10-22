package accountpolicy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func splitFixture(t *testing.T) (*Service, Options, *fakeProvider, *time.Time) {
	t.Helper()
	now := testTime()
	p := &fakeProvider{snapshots: map[string]Snapshot{"a": testSnapshot("a", 4), "b": testSnapshot("b", 7)}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, opts := fixtureService(t, p, &now, settings)
	return s, opts, p, &now
}

func TestSplitOverrideValidationPersistenceAndExpiry(t *testing.T) {
	s, opts, _, now := splitFixture(t)
	for _, shares := range [][]RoutingShare{
		nil, {{"a", 50}}, {{"a", 50}, {"a", 50}}, {{"a", 0}, {"b", 100}},
		{{"a", -1}, {"b", 101}}, {{"a", 50}, {"b", 49}}, {{"a", 50}, {"missing", 50}},
		{{"a", 40}, {"b", 40}, {"c", 20}},
	} {
		if _, err := s.ReplaceSplitOverride(shares, 2, 0); err == nil {
			t.Fatalf("accepted invalid split %+v", shares)
		}
	}
	for _, hours := range []int{0, 25} {
		if _, err := s.ReplaceSplitOverride([]RoutingShare{{"a", 50}, {"b", 50}}, hours, 0); err == nil {
			t.Fatalf("accepted %d hours", hours)
		}
	}
	active, err := s.ReplaceSplitOverride([]RoutingShare{{"a", 1}, {"b", 99}}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if active.Mode != "split" || len(active.Accounts) != 2 || active.Accounts[0].Percent != 1 || active.Accounts[1].Percent != 99 || !active.ExpiresAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("incorrect split %+v", active)
	}
	if _, err := s.ReplaceSplitOverride([]RoutingShare{{"a", 99}, {"b", 1}}, 1, 0); err == nil {
		t.Fatal("accepted stale revision")
	}
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	id, shares := restarted.RoutingSplit("codex")
	if id != active.ID || len(shares) != 2 || shares[1].Percent != 99 || restarted.PreferredAccount("codex") != "" {
		t.Fatalf("restart lost split: %q %+v", id, shares)
	}
	*now = now.Add(2 * time.Hour)
	if id, _ := restarted.RoutingSplit("codex"); id != "" {
		t.Fatal("expired split still active")
	}
	if err := restarted.MaintainOverride(); err != nil {
		t.Fatal(err)
	}
	if restarted.RoutingOverride().State != "expired" {
		t.Fatal("split expiry not persisted")
	}
}

func TestSplitOverrideExhaustionAndIdentitySafetyForBothParticipants(t *testing.T) {
	for _, target := range []string{"a", "b"} {
		for _, reason := range []string{"quota", "signal", "removed", "reassigned"} {
			t.Run(target+"/"+reason, func(t *testing.T) {
				s, _, p, _ := splitFixture(t)
				if _, err := s.ReplaceSplitOverride([]RoutingShare{{"a", 37}, {"b", 63}}, 2, 0); err != nil {
					t.Fatal(err)
				}
				old := p.snapshots[target]
				switch reason {
				case "quota":
					next := clone(old)
					next.Buckets[0].UsedPercent = 100
					p.snapshots[target] = next
					if err := s.Refresh(context.Background(), target); err != nil {
						t.Fatal(err)
					}
				case "signal":
					if err := s.ConfirmOverrideExhaustion(old.Identity); err != nil {
						t.Fatal(err)
					}
				case "removed":
					delete(p.snapshots, target)
				case "reassigned":
					next := old
					next.Identity.AccountID = "different-owner"
					p.snapshots[target] = next
				}
				if id, _ := s.RoutingSplit("codex"); id != "" {
					t.Fatal("unsafe split remained effective")
				}
				if err := s.MaintainOverride(); err != nil {
					t.Fatal(err)
				}
				want := "unavailable"
				if reason == "quota" || reason == "signal" {
					want = "exhausted"
				}
				if s.RoutingOverride().State != want {
					t.Fatalf("state %+v", s.RoutingOverride())
				}
			})
		}
	}
}

func TestSplitReplacementCancellationAndTemporaryRefresh(t *testing.T) {
	s, _, p, _ := splitFixture(t)
	active, err := s.ReplaceSplitOverride([]RoutingShare{{"a", 50}, {"b", 50}}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]Identity{"a": p.snapshots["a"].Identity, "b": p.snapshots["b"].Identity}
	s.opts.OverrideAccountState = func(id string) (Identity, string) { return identities[id], "temporary" }
	delete(p.snapshots, "b")
	if err := s.MaintainOverride(); err != nil || s.RoutingOverride().State != "active" {
		t.Fatalf("refresh ended split: %v", err)
	}
	single, err := s.ReplaceOverride("a", 1, active.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := s.RoutingSplit("codex"); id != "" {
		t.Fatal("split survived replacement")
	}
	if s.PreferredAccount("codex") != "a" {
		t.Fatal("single replacement ineffective")
	}
	if _, err := s.CancelOverride(single.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedStoredSplitIsRejected(t *testing.T) {
	s, opts, _, _ := splitFixture(t)
	if _, err := s.ReplaceSplitOverride([]RoutingShare{{"a", 50}, {"b", 50}}, 1, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Settings.StateDir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state["routing_override"].(map[string]any)["accounts"].([]any)[0].(map[string]any)["percent"] = 0
	raw, _ = json.Marshal(state)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(opts); err == nil {
		t.Fatal("accepted invalid persisted split")
	}
}
