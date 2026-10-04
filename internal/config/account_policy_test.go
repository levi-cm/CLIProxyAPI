package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func policyJSON(t *testing.T, cfg *Config) map[string]any {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	policy, ok := value["account-policy"].(map[string]any)
	if !ok {
		t.Fatal("account-policy settings missing from effective configuration")
	}
	return policy
}

func TestAccountPolicyFileLoadingAndOptionalDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if policyJSON(t, cfg)["automation"] != "off" {
		t.Fatal("optional config lost safe defaults")
	}
	if err = os.WriteFile(path, []byte("port: 8317\naccount-policy: {enabled: true, automation: notify}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := policyJSON(t, cfg); p["enabled"] != true || p["automation"] != "notify" {
		t.Fatalf("file loading discarded policy: %#v", p)
	}
}

func TestAccountPolicyCloneDoesNotShareAccountControls(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\naccount-policy:\n  accounts: {B: {hold: true}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cloned := cfg.CloneForRuntime()
	control := cloned.AccountPolicy.Accounts["B"]
	control.Hold = false
	cloned.AccountPolicy.Accounts["B"] = control
	cloned.AccountPolicy.CreditTypes[0] = "changed"
	if !cfg.AccountPolicy.Accounts["B"].Hold || cfg.AccountPolicy.CreditTypes[0] != "codex_rate_limits" {
		t.Fatal("runtime clone shares mutable policy state")
	}
}

func TestAccountPolicyConfigurationDefaultsOff(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := policyJSON(t, cfg)
	if p["enabled"] != false || p["automation"] != "off" || p["expiry_guard_seconds"] != float64(600) || p["freshness_seconds"] != float64(120) {
		t.Fatalf("unsafe or incomplete defaults: %#v", p)
	}
}

func TestAccountPolicyConfigurationRoundTrip(t *testing.T) {
	for _, prefix := range []string{"port: 8317\n", "config-version: 8\nserver: {port: 8317}\n"} {
		cfg, err := ParseConfigBytes([]byte(prefix + "account-policy:\n  enabled: true\n  automation: notify\n  state-dir: /tmp/test-policy\n  time-zone: Europe/Berlin\n  accounts:\n    B: {hold: true, reserve-percent: 12}\n"))
		if err != nil {
			t.Fatal(err)
		}
		p := policyJSON(t, cfg)
		if p["enabled"] != true || p["automation"] != "notify" || p["state_dir"] != "/tmp/test-policy" {
			t.Fatalf("configuration discarded policy: %#v", p)
		}
		cloned := cfg.CloneForRuntime()
		original, _ := json.Marshal(cfg)
		copyData, _ := json.Marshal(cloned)
		if string(original) != string(copyData) {
			t.Fatal("runtime clone changed policy")
		}
	}
}

func TestAccountPolicyRejectsUnsafeConfiguration(t *testing.T) {
	for _, content := range []string{
		"automation: purchase", "freshness-seconds: 121", "expiry-guard-seconds: -1",
		"accounts: {B: {reserve-percent: 101}}", "time-zone: Mars/Olympus",
	} {
		_, err := ParseConfigBytes([]byte("port: 8317\naccount-policy: {" + content + "}\n"))
		if err == nil {
			t.Errorf("accepted invalid policy %s", content)
		}
	}
}
