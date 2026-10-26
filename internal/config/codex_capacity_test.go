package config

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
	"time"
)

func TestCodexCapacityConfigDefaultsAndV8RoundTrip(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("server: {port: 8317}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.CapacityRetry.Enabled {
		t.Fatal("extended recovery must be opt-in")
	}
	defaults := cfg.Codex.CapacityRetry.WithDefaults()
	if defaults.Budget() != 30*time.Minute || defaults.MaxAttempts != 8 || defaults.MaxPending != 16 || defaults.MaxPendingPerAccount != 8 || defaults.MaxPendingBytes != 128<<20 || defaults.MinAvailableMemoryBytes != 1<<30 {
		t.Fatalf("defaults: %+v", defaults)
	}
	raw := []byte("upstream:\n  codex:\n    capacity-retry:\n      enabled: true\n      total-budget: 30m\n      max-attempts: 8\naccount-policy:\n  automatic-spreading-enabled: false\n")
	cfg, err = ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Codex.CapacityRetry.Enabled || cfg.AccountPolicy.AutomaticSpreadingEnabled == nil || *cfg.AccountPolicy.AutomaticSpreadingEnabled {
		t.Fatal("settings lost")
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err = NormalizeConfigLayout(encoded, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "capacity-retry:") || !strings.Contains(string(encoded), "upstream:") {
		t.Fatalf("v8 serialization lost block: %s", encoded)
	}
	again, err := ParseConfigBytes(encoded)
	if err != nil || !again.Codex.CapacityRetry.Enabled {
		t.Fatalf("round trip: %v", err)
	}
	cloned := cfg.CloneForRuntime()
	*cloned.AccountPolicy.AutomaticSpreadingEnabled = true
	if *cfg.AccountPolicy.AutomaticSpreadingEnabled {
		t.Fatal("clone shared mutable settings")
	}
}

func TestCodexCapacityRejectsUnboundedSettings(t *testing.T) {
	for _, value := range []string{"total-budget: 31m", "total-budget: -1s", "total-budget: nonsense", "max-attempts: 9", "max-pending: -1", "max-pending-per-account: 17", "max-pending-bytes: -1", "min-available-memory-bytes: -1"} {
		_, err := ParseConfigBytes([]byte("upstream:\n  codex:\n    capacity-retry:\n      enabled: true\n      " + value + "\n"))
		if err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}
