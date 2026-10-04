package cliproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type lastPolicySelector struct{}

func (*lastPolicySelector) Pick(_ context.Context, _, _ string, _ coreexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return auths[len(auths)-1], nil
}

func TestAccountPolicyDisabledPreservesCustomSelector(t *testing.T) {
	manager := coreauth.NewManager(nil, &lastPolicySelector{}, nil)
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.StateDir = t.TempDir()
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithCoreAuthManager(manager).WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
	if err != nil {
		t.Fatal(err)
	}
	candidates := []*coreauth.Auth{{ID: "A"}, {ID: "B"}}
	selected, err := service.coreManager.Selector().Pick(context.Background(), "codex", "fixture-model", coreexecutor.Options{}, candidates)
	if err != nil || selected == nil || selected.ID != "B" {
		t.Fatalf("disabled policy replaced embedding selector: %#v %v", selected, err)
	}
}

func TestAccountPolicyBuilderRejectsHomeOwnership(t *testing.T) {
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.Enabled = true
	cfg.Home.Enabled = true
	_, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").Build()
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("conflicting ownership accepted: %v", err)
	}
}

func TestAccountPolicyRejectsNewSchedulerBeforeRuntimeAssignment(t *testing.T) {
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.Enabled = true
	cfg.AccountPolicy.StateDir = t.TempDir()
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
	if err != nil {
		t.Fatal(err)
	}
	candidate := cfg.CloneForRuntime()
	candidate.Plugins.Enabled = true
	candidate.Debug = true
	service.cfg = candidate
	if !service.rejectConflictingPolicyPlugins(context.Background(), candidate, true) {
		t.Fatal("new scheduler accepted while account policy owns selection")
	}
	if service.cfg.Plugins.Enabled || !service.cfg.Debug {
		t.Fatal("rejected plugin section did not restore safe plugin ownership")
	}
	if !service.accountPolicy.Settings().Enabled {
		t.Fatal("scheduler conflict silently disabled active policy")
	}
}

type policyFixtureProvider struct{ discovered chan string }

func (p *policyFixtureProvider) Discover(_ context.Context, id accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	if p.discovered != nil {
		p.discovered <- id.CredentialID
	}
	now := time.Now().UTC()
	return accountpolicy.Snapshot{Identity: id, Status: "healthy", Eligible: true, ObservedAt: now, InventoryObservedAt: now, InventoryComplete: true}, nil
}
func (*policyFixtureProvider) Consume(context.Context, accountpolicy.Identity, string, string) (accountpolicy.ConsumeResult, error) {
	return accountpolicy.ConsumeResult{Code: "nothing_to_reset"}, nil
}

func TestAccountPolicyLifecycleDisabledAndDynamicEnable(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	policyTestAuth(t, manager, "credential-B", "account-B")
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.StateDir = t.TempDir()
	fixture := &policyFixtureProvider{discovered: make(chan string, 10)}
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithCoreAuthManager(manager).WithAccountPolicyProvider(fixture).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err = service.accountPolicy.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.discovered:
		t.Fatal("disabled module polled provider")
	default:
	}
	settings := service.accountPolicy.Settings()
	settings.Enabled = true
	if err = service.accountPolicy.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err = service.accountPolicy.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if id := <-fixture.discovered; id != "credential-B" {
		t.Fatalf("discovery wrong account %q", id)
	}
	if snapshot, ok := service.accountPolicy.Snapshot("credential-B"); !ok || snapshot.Transport == "websocket" {
		t.Fatalf("missing or invented runtime evidence %#v", snapshot)
	}
}

func TestAccountPolicyReloadKeepsPersistedOperatorControls(t *testing.T) {
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.Enabled = true
	cfg.AccountPolicy.StateDir = t.TempDir()
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
	if err != nil {
		t.Fatal(err)
	}
	settings := service.accountPolicy.Settings()
	settings.Accounts["B"] = accountpolicy.AccountControl{Hold: true}
	if err = service.accountPolicy.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	reloaded := cfg.CloneForRuntime()
	reloaded.Debug = true
	if commit := service.commitConfigUpdate(reloaded); commit.cfg == nil {
		t.Fatal("valid reload rejected")
	}
	if !service.accountPolicy.Settings().Accounts["B"].Hold {
		t.Fatal("unrelated config reload erased persisted operator control")
	}
	reloaded = reloaded.CloneForRuntime()
	reloaded.Home.Enabled = true
	if commit := service.commitConfigUpdate(reloaded); commit.cfg != nil {
		t.Fatal("hot reload permitted Home policy conflict")
	}
}

func TestAccountPolicyBackgroundCredentialWakeAndCancellation(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	policyTestAuth(t, manager, "credential-B", "account-B")
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.Enabled = true
	cfg.AccountPolicy.StateDir = t.TempDir()
	fixture := &policyFixtureProvider{discovered: make(chan string, 10)}
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithCoreAuthManager(manager).WithAccountPolicyProvider(fixture).Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.startAccountPolicy(ctx)
	defer service.stopAccountPolicy()
	select {
	case id := <-fixture.discovered:
		if id != "credential-B" {
			t.Fatal(id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup discovery did not run")
	}
	policyTestAuth(t, manager, "credential-C", "account-C")
	service.wakeAccountPolicy("credential-C")
	select {
	case id := <-fixture.discovered:
		if id != "credential-C" {
			t.Fatal(id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credential change did not refresh independently")
	}
	service.accountPolicyMu.Lock()
	done := service.accountPolicyDone
	service.accountPolicyMu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("policy background worker survived lifecycle cancellation")
	}
}

func policyTestAuth(t *testing.T, manager *coreauth.Manager, id, account string) {
	t.Helper()
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"` + account + `"}}`))
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Label: id, Metadata: map[string]any{"access_token": "secret-access", "refresh_token": "secret-refresh", "account_id": account, "id_token": "e30." + claims + ".signature"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAccountPolicyCredentialMappingAndSanitizedIdentity(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	policyTestAuth(t, manager, "credential-B", "account-B")
	service := &Service{coreManager: manager}
	identities := service.policyAccountIdentities()
	if len(identities) != 1 || identities[0].AccountID != "account-B" || identities[0].WorkspaceID != "account-B" || identities[0].CredentialID != "credential-B" {
		t.Fatalf("incorrect identity mapping: %#v", identities)
	}
	data, err := json.Marshal(identities)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-") || strings.Contains(string(data), "signature") {
		t.Fatal("credential material exposed in identity")
	}
	credential, err := service.policyCredential(context.Background(), "credential-B")
	if err != nil || credential.AccountID != "account-B" || credential.AccessToken != "secret-access" {
		t.Fatalf("credential lookup failed: %#v %v", credential, err)
	}
}

func TestAccountPolicyCredentialRejectsIdentityConflict(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	policyTestAuth(t, manager, "credential-B", "account-B")
	auth, _ := manager.GetByID("credential-B")
	auth.Metadata["account_id"] = "account-A"
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{coreManager: manager}
	_, err := service.policyCredential(context.Background(), "credential-B")
	if err == nil {
		t.Fatal("account metadata and claims conflict permitted")
	}
	if strings.Contains(err.Error(), "secret-") {
		t.Fatal("error leaked credentials")
	}
}

func TestAccountPolicyDiscoveryIncludesQuotaCooldown(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	policyTestAuth(t, manager, "credential-B", "account-B")
	auth, _ := manager.GetByID("credential-B")
	auth.Unavailable = true
	auth.Status = coreauth.StatusError
	auth.Quota.Exceeded = true
	auth.LastError = &coreauth.Error{HTTPStatus: 429, Code: "quota"}
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{coreManager: manager}
	if identities := service.policyAccountIdentities(); len(identities) != 1 {
		t.Fatalf("cooldown account excluded from discovery: %#v", identities)
	}
}
