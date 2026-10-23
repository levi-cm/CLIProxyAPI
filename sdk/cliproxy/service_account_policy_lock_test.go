package cliproxy

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"testing"
)

func TestAutomaticRedemptionConfigReloadCannotDisable(t *testing.T) {
	settings := accountpolicy.DefaultSettings()
	settings.Enabled, settings.Automation = true, "auto_expiring"
	settings.StateDir = t.TempDir()
	cfg := &config.Config{AccountPolicy: settings}
	s, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*accountpolicy.Settings){
		func(v *accountpolicy.Settings) { v.Automation = "off" },
		func(v *accountpolicy.Settings) { v.Enabled = false },
		func(v *accountpolicy.Settings) { v.ReadOnly = true },
		func(v *accountpolicy.Settings) { v.CreditTypes = nil; v.ExpiryGuardSeconds = 0 },
	} {
		next := cfg.CloneForRuntime()
		change(&next.AccountPolicy)
		_ = s.commitConfigUpdate(next)
		current := s.accountPolicy.Settings()
		if !current.Enabled || current.Automation != "auto_expiring" || current.ReadOnly || current.ExpiryGuardSeconds < 600 || len(current.CreditTypes) != 1 {
			t.Fatalf("reload disabled mandatory automation: %+v", current)
		}
	}
}
