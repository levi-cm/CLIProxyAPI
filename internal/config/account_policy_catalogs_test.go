package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountPolicyAndUpstreamModelCatalogsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("config-version: 8\nserver: {port: 8317}\nmodels:\n  codex-catalog: https://example.com/codex.json\naccount-policy:\n  enabled: true\n  observations-enabled: true\n  automation: off\n")
	if err := ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Models.CodexCatalog != "https://example.com/codex.json" || !cfg.AccountPolicy.Enabled || !cfg.AccountPolicy.ObservationsEnabled || cfg.AccountPolicy.Automation != "off" {
			t.Fatal("upstream catalogs and optional policy did not retain independent settings")
		}
		if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateV8Config(saved); err != nil {
			t.Fatal(err)
		}
	}
}
