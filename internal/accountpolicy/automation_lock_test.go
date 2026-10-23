package accountpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func automaticFixture(t *testing.T) (*Service, Options, *fakeProvider) {
	t.Helper()
	now := testTime()
	snapshot := testSnapshot("a", 7)
	expiry := now.Add(9 * time.Minute)
	snapshot.Credits = []Credit{testCredit("expiring", &expiry)}
	snapshot.AvailableCredits = 1
	p := &fakeProvider{snapshots: map[string]Snapshot{"a": snapshot}}
	settings := DefaultSettings()
	settings.Enabled, settings.Automation = true, "auto_expiring"
	s, opts := fixtureService(t, p, &now, settings)
	return s, opts, p
}

func TestAutomaticRedemptionCannotBeDisabled(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*Settings)
	}{
		{"off", func(s *Settings) { s.Automation = "off" }},
		{"notify", func(s *Settings) { s.Automation = "notify" }},
		{"policy-disabled", func(s *Settings) { s.Enabled = false }},
		{"read-only", func(s *Settings) { s.ReadOnly = true }},
		{"no-credit-types", func(s *Settings) { s.CreditTypes = nil }},
		{"no-expiry-guard", func(s *Settings) { s.ExpiryGuardSeconds = 0 }},
		{"remove-lock", func(s *Settings) { _ = json.Unmarshal([]byte(`{"automation_required":false}`), s) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s, opts, _ := automaticFixture(t)
			original := s.Settings()
			journal := filepath.Join(opts.Settings.StateDir, "state.json")
			before, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			next := clone(original)
			change.apply(&next)
			err = s.UpdateSettings(next)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "automation_required" {
				t.Fatalf("disable attempt accepted or wrong error: %v", err)
			}
			after, err := os.ReadFile(journal)
			if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(original, s.Settings()) {
				t.Fatalf("rejected attempt changed durable/live settings: %v", err)
			}
		})
	}
}

func TestAutomaticRedemptionRemainsRequiredAfterRestartWithDisabledConfig(t *testing.T) {
	_, opts, p := automaticFixture(t)
	// Simulate a stale startup file that attempts to turn the feature off.
	opts.Settings.Enabled, opts.Settings.Automation = false, "off"
	opts.Settings.ExpiryGuardSeconds = 0
	restarted, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	settings := restarted.Settings()
	var fields map[string]any
	raw, _ := json.Marshal(settings)
	_ = json.Unmarshal(raw, &fields)
	if !settings.Enabled || settings.Automation != "auto_expiring" || fields["automation_required"] != true || settings.ReadOnly || settings.ExpiryGuardSeconds < 600 {
		t.Fatalf("startup disabled mandatory redemption: %+v", settings)
	}
	// Prove the scheduler still redeems a useful imminent reset without demand.
	restarted.opts.HasDemand = func(string) bool { return false }
	p.consume = func(id Identity, _, credit string) (ConsumeResult, error) {
		if id.CredentialID != "a" || credit != "expiring" {
			return ConsumeResult{}, errors.New("wrong reset owner")
		}
		after := clone(p.snapshots["a"])
		after.Credits = nil
		after.AvailableCredits = 0
		for i := range after.Buckets {
			after.Buckets[i].UsedPercent = 0
		}
		p.snapshots["a"] = after
		return ConsumeResult{Code: "reset", WindowsReset: 1}, nil
	}
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if operations := restarted.Operations(); len(operations) != 1 || operations[0].State != "confirmed" {
		t.Fatalf("expiring reset was not redeemed: %+v", operations)
	}
	// Read-only replicas must never become competing reset writers.
	opts.Settings.ReadOnly = true
	replica, err := NewService(opts)
	if err != nil || !replica.Settings().ReadOnly {
		t.Fatalf("replica gained authority: %v", err)
	}
}

func TestAutomaticRedemptionActivationLatchesAndAllowsOtherSettings(t *testing.T) {
	now := testTime()
	p := &fakeProvider{snapshots: map[string]Snapshot{"a": testSnapshot("a", 7)}}
	settings := DefaultSettings()
	settings.Enabled = true
	s, _ := fixtureService(t, p, &now, settings)
	settings.Automation = "auto_expiring"
	settings.StateDir = s.Settings().StateDir
	if err := s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	settings = s.Settings()
	settings.Automation, settings.Fallback = "auto_all_saved", "fill-first"
	if err := s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	settings = s.Settings()
	settings.Automation = "off"
	if err := s.UpdateSettings(settings); err == nil {
		t.Fatal("later update cleared permanent requirement")
	}
}

func TestAutomaticRedemptionMigratesLegacyJournalWithoutConstructorWrites(t *testing.T) {
	_, opts, _ := automaticFixture(t)
	path := filepath.Join(opts.Settings.StateDir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(legacy["settings"], &settings); err != nil {
		t.Fatal(err)
	}
	delete(settings, "automation_required")
	legacy["settings"], _ = json.Marshal(settings)
	raw, _ = json.Marshal(legacy)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	opts.Settings.Automation, opts.Settings.Enabled = "off", false
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Settings().AutomationRequired || !s.Settings().Enabled || s.Settings().Automation != "auto_expiring" {
		t.Fatal("legacy active automation was disabled")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("constructor wrote shared policy journal")
	}
	if err := s.MergeSettings(func(v Settings) (Settings, error) { v.Fallback = "fill-first"; return v, nil }); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Contains(after, []byte(`"automation_required":true`)) {
		t.Fatal("authoritative settings commit did not persist migrated lock")
	}
}

func TestRequiredAutomaticRedemptionDoesNotGiveReplicasWriteAuthority(t *testing.T) {
	_, opts, p := automaticFixture(t)
	opts.Settings.ReadOnly = true
	replica, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 0 {
		t.Fatal("mandatory automation redeemed from a read-only replica")
	}
}

func TestAutomaticRedemptionStartupLockPersistsWithoutAccounts(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "previously-off"}[existing], func(t *testing.T) {
			settings := DefaultSettings()
			settings.StateDir = t.TempDir()
			opts := Options{Settings: settings, Provider: &fakeProvider{}, Accounts: func() []Identity { return nil }}
			if existing {
				off, err := NewService(opts)
				if err != nil {
					t.Fatal(err)
				}
				if err := off.UpdateSettings(settings); err != nil {
					t.Fatal(err)
				}
			}
			opts.Settings.Automation = "auto_expiring"
			opts.Settings.Enabled = true
			s, err := NewService(opts)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.Run(ctx)
			opts.Settings.Automation = "off"
			opts.Settings.Enabled = false
			restarted, err := NewService(opts)
			if err != nil {
				t.Fatal(err)
			}
			if current := restarted.Settings(); !current.Enabled || !current.AutomationRequired || current.Automation != "auto_expiring" {
				t.Fatalf("zero-account startup lost durable automatic redemption: %+v", current)
			}
		})
	}
}

func TestAutomaticRedemptionStartupLockCannotWriteAsSecondOwner(t *testing.T) {
	settings := DefaultSettings()
	settings.StateDir = t.TempDir()
	settings.Enabled, settings.Automation = true, "auto_expiring"
	release, err := lockNamed(settings.StateDir, "owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s, err := NewService(Options{Settings: settings, Provider: &fakeProvider{}, Accounts: func() []Identity { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if !s.Settings().ReadOnly {
		t.Fatal("second owner gained reset write authority")
	}
	if _, err := os.Stat(filepath.Join(settings.StateDir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("second owner wrote policy journal: %v", err)
	}
}
