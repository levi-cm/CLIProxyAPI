package cliproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

func (s *Service) validateAccountPolicySettings(settings accountpolicy.Settings) error {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if err := validatePolicyOwnership(cfg, settings, s.pluginHost != nil && s.pluginHost.HasScheduler()); err != nil {
		return err
	}
	if settings.Enabled && s.accountPolicy != nil {
		s.ensureAccountPolicyRouting(cfg)
	}
	return nil
}

func (s *Service) ensureAccountPolicyRouting(cfg *config.Config) {
	s.accountPolicyMu.Lock()
	defer s.accountPolicyMu.Unlock()
	if s.accountPolicyRoutingInstalled.Load() || s.accountPolicy == nil {
		return
	}
	s.accountPolicyRoutingInstalled.Store(true)
	state := normalizedRoutingRuntimeState(cfg)
	s.coreManager.SetSelector(s.newPolicyRoutingSelector(state))
}

func (s *Service) initializeAccountPolicy() error {
	if s == nil || s.cfg == nil {
		return nil
	}
	settings := normalizedPolicySettings(s.cfg.AccountPolicy)
	s.accountPolicyConfig = settings
	s.accountPolicyPluginConfig = s.cfg.CloneForRuntime()
	if settings.StateDir == "" {
		settings.StateDir = filepath.Join(filepath.Dir(s.configPath), "account-policy-state")
	}
	provider := s.accountPolicyProvider
	if provider == nil {
		provider = &accountpolicy.CodexClient{
			Credential: s.policyCredential,
			RefreshCredential: func(ctx context.Context, id string) error {
				_, err := s.coreManager.ForceRefreshAuth(ctx, id)
				if err != nil {
					return &accountpolicy.Error{Code: "authentication_failed", Message: "credential refresh failed"}
				}
				return nil
			},
			HTTPClientForCredential: func(id string) *http.Client { return &http.Client{Transport: s.coreManager.CredentialRoundTripper(id)} },
		}
	}
	policy, err := accountpolicy.NewService(accountpolicy.Options{
		Settings: settings, Provider: &policyRuntimeProvider{service: s, provider: provider},
		Accounts: s.policyAccountIdentities, Recover: s.coreManager.RecoverPolicyQuota,
		IsIdle: s.coreManager.PolicyIsIdle, HasDemand: s.coreManager.PolicyHasDemand,
		AcquireReset: s.coreManager.AcquirePolicyReset,
	})
	if err != nil {
		if !settings.Enabled {
			log.WithField("code", "account_policy_unavailable").Warn("disabled account policy state is unavailable; normal proxy startup continues and policy management remains unavailable")
			return nil
		}
		return err
	}
	s.accountPolicy = policy
	s.coreManager.SetPolicyRefreshCallback(policy.RequestRefresh)
	s.accountPolicyDisabledFallback = s.coreManager.Selector()
	if err := s.validateAccountPolicySettings(policy.Settings()); err != nil {
		return err
	}
	return nil
}

func (s *Service) newPolicyRoutingSelector(state routingRuntimeState) coreauth.Selector {
	if s.accountPolicy == nil || !s.accountPolicyRoutingInstalled.Load() {
		return newRoutingSelector(state)
	}
	bare := state
	bare.sessionAffinity = false
	selector := coreauth.NewEarliestDeadlineSelector(s.accountPolicy, newRoutingSelector(bare))
	selector.SetIdleCheck(s.coreManager.PolicyIsIdle)
	selector.SetModelResolver(s.coreManager.PolicyModelForAuth)
	selector.SetBindingAuthResolver(s.coreManager.GetByID)
	disabledFallback := s.accountPolicyDisabledFallback
	if disabledFallback == nil {
		disabledFallback = newRoutingSelector(state)
	}
	selector.SetDisabledFallback(disabledFallback)
	return selector
}

// rejectConflictingPolicyPlugins restores the last accepted plugin section before
// a newly loaded scheduler can become an active account-selection owner.
func (s *Service) rejectConflictingPolicyPlugins(ctx context.Context, candidate *config.Config, hasScheduler bool) bool {
	if s == nil || s.accountPolicy == nil || !s.accountPolicy.Settings().Enabled || !hasScheduler {
		return false
	}
	s.accountPolicyMu.Lock()
	previous := s.accountPolicyPluginConfig
	s.accountPolicyMu.Unlock()
	if previous == nil {
		previous = &config.Config{}
	}
	if s.pluginHost != nil {
		s.pluginHost.ApplyConfig(ctx, previous)
	}
	s.cfgMu.Lock()
	if s.cfg == candidate && candidate != nil {
		restored := candidate.CloneForRuntime()
		restored.Plugins = previous.CloneForRuntime().Plugins
		s.cfg = restored
		if s.coreManager != nil {
			s.coreManager.SetConfig(restored)
		}
	}
	s.cfgMu.Unlock()
	log.WithField("code", "ownership_conflict").Warn("rejected scheduler plugin configuration: active account policy owns selection; previous plugin configuration restored")
	return true
}

func (s *Service) startAccountPolicy(ctx context.Context) error {
	if s.accountPolicy == nil {
		return nil
	}
	settings := s.accountPolicy.Settings()
	sink, err := accountpolicyusage.New(settings.StateDir, func() bool {
		current := s.accountPolicy.Settings()
		return current.Enabled || current.ObservationsEnabled
	})
	if err != nil {
		return &accountpolicy.Error{Code: "usage_state_unavailable", Message: "cannot initialize durable usage state"}
	}
	s.accountPolicyMu.Lock()
	defer s.accountPolicyMu.Unlock()
	if s.accountPolicy == nil || s.accountPolicyCancel != nil {
		_ = sink.Close()
		return nil
	}
	s.accountPolicyUsage = sink
	usage.RegisterNamedPlugin("account-policy-durable-usage", sink)
	policyCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.accountPolicyCancel, s.accountPolicyDone = cancel, done
	go func() {
		defer close(done)
		s.accountPolicy.Run(policyCtx)
	}()
	return nil
}

func (s *Service) accountPolicyUsageSink() *accountpolicyusage.Sink {
	s.accountPolicyMu.Lock()
	defer s.accountPolicyMu.Unlock()
	return s.accountPolicyUsage
}

func (s *Service) closeAccountPolicyUsage(ctx context.Context) {
	s.accountPolicyMu.Lock()
	sink := s.accountPolicyUsage
	s.accountPolicyUsage = nil
	s.accountPolicyMu.Unlock()
	if sink == nil {
		return
	}
	usage.StopDefault()
	if errWait := usage.WaitDefault(ctx); errWait != nil {
		log.WithField("component", "account-policy-usage").Warn("usage drain continues after service shutdown context ended")
		go func() {
			if usage.WaitDefault(context.Background()) == nil {
				if errClose := sink.Close(); errClose != nil {
					log.WithField("component", "account-policy-usage").Error("durable usage close failed")
				}
			}
		}()
		return
	}
	if errClose := sink.Close(); errClose != nil {
		log.WithField("component", "account-policy-usage").Error("durable usage close failed")
	}
}

func (s *Service) wakeAccountPolicy(id string) {
	if s == nil || s.accountPolicy == nil || id == "" {
		return
	}
	s.accountPolicy.RequestRefresh(id)
}

func (s *Service) stopAccountPolicy() {
	s.accountPolicyMu.Lock()
	cancel, done := s.accountPolicyCancel, s.accountPolicyDone
	s.accountPolicyCancel, s.accountPolicyDone = nil, nil
	s.accountPolicyMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if s.coreManager != nil {
		s.coreManager.SetPolicyRefreshCallback(nil)
	}
}

type policyRuntimeProvider struct {
	service  *Service
	provider accountpolicy.Provider
}

func (p *policyRuntimeProvider) Discover(ctx context.Context, identity accountpolicy.Identity) (accountpolicy.Snapshot, error) {
	snapshot, err := p.provider.Discover(ctx, identity)
	if err != nil {
		return snapshot, err
	}
	auth, ok := p.service.coreManager.GetByID(identity.CredentialID)
	if !ok || policyAuthUnavailable(auth) {
		snapshot.Status, snapshot.Eligible, snapshot.WritesDisabled = "authentication_unavailable", false, true
	}
	runtime := p.service.coreManager.PolicyRuntime(identity.CredentialID)
	snapshot.ActiveRequests, snapshot.ActiveBindings, snapshot.Transport = runtime.ActiveRequests, runtime.ActiveBindings, runtime.Transport
	return snapshot, nil
}

func (p *policyRuntimeProvider) Consume(ctx context.Context, identity accountpolicy.Identity, requestID, creditID string) (accountpolicy.ConsumeResult, error) {
	return p.provider.Consume(ctx, identity, requestID, creditID)
}

func validatePolicyOwnership(cfg *config.Config, settings accountpolicy.Settings, hasScheduler bool) error {
	if err := accountpolicy.ValidateSettings(settings); err != nil {
		return err
	}
	if settings.Enabled && ((cfg != nil && cfg.Home.Enabled) || hasScheduler) {
		return &accountpolicy.Error{Code: "ownership_conflict", Message: "account policy ownership conflicts with Home or a scheduler plugin"}
	}
	return nil
}

func normalizedPolicySettings(settings accountpolicy.Settings) accountpolicy.Settings {
	if settings.Mode == "" {
		defaults := accountpolicy.DefaultSettings()
		settings.Mode = defaults.Mode
		if settings.Automation == "" {
			settings.Automation = defaults.Automation
		}
		if settings.Fallback == "" {
			settings.Fallback = defaults.Fallback
		}
		if settings.Affinity == "" {
			settings.Affinity = defaults.Affinity
		}
		if settings.TimeZone == "" {
			settings.TimeZone = defaults.TimeZone
		}
		if settings.ExpiryGuardSeconds == 0 {
			settings.ExpiryGuardSeconds = defaults.ExpiryGuardSeconds
		}
		if settings.FreshnessSeconds == 0 {
			settings.FreshnessSeconds = defaults.FreshnessSeconds
		}
		if settings.CreditTypes == nil {
			settings.CreditTypes = defaults.CreditTypes
		}
		if settings.Accounts == nil {
			settings.Accounts = defaults.Accounts
		}
		if settings.SavedCreditReserve == 0 {
			settings.SavedCreditReserve = defaults.SavedCreditReserve
		}
	}
	return settings
}

func policyAuthUnavailable(auth *coreauth.Auth) bool {
	return auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.Status == coreauth.StatusPending || auth.Status == coreauth.StatusRefreshing || auth.Status == coreauth.StatusUnknown || (auth.LastError != nil && (auth.LastError.HTTPStatus == 401 || auth.LastError.HTTPStatus == 403 || auth.LastError.Code == "unauthorized"))
}

func policyMetadata(auth *coreauth.Auth, name string) string {
	if auth == nil {
		return ""
	}
	value, _ := auth.Metadata[name].(string)
	return strings.TrimSpace(value)
}

func policyIdentity(auth *coreauth.Auth) (accountpolicy.Identity, error) {
	return coreauth.PolicyAccountIdentity(auth)
}

func (s *Service) policyAccountIdentities() []accountpolicy.Identity {
	if s == nil || s.coreManager == nil {
		return nil
	}
	identities := make([]accountpolicy.Identity, 0)
	for _, auth := range s.coreManager.List() {
		if policyAuthUnavailable(auth) {
			continue
		}
		identity, err := policyIdentity(auth)
		if err == nil {
			identities = append(identities, identity)
		}
	}
	return identities
}

func (s *Service) policyCredential(ctx context.Context, credentialID string) (accountpolicy.CodexCredential, error) {
	credential := accountpolicy.CodexCredential{}
	if s == nil || s.coreManager == nil {
		return credential, &accountpolicy.Error{Code: "invalid_account", Message: "credential manager is unavailable"}
	}
	auth, ok := s.coreManager.GetByID(credentialID)
	if !ok || policyAuthUnavailable(auth) {
		return credential, &accountpolicy.Error{Code: "invalid_account", Message: "credential is disabled or requires repair"}
	}
	if !auth.HasValidAccessToken(time.Now()) {
		refreshed, err := s.coreManager.ForceRefreshAuth(ctx, credentialID)
		if err != nil {
			return credential, &accountpolicy.Error{Code: "authentication_failed", Message: "credential refresh failed"}
		}
		auth = refreshed
	}
	identity, err := policyIdentity(auth)
	if err != nil {
		return credential, err
	}
	token := policyMetadata(auth, "access_token")
	if token == "" {
		return credential, &accountpolicy.Error{Code: "authentication_failed", Message: "access token is unavailable"}
	}
	return accountpolicy.CodexCredential{AccessToken: token, AccountID: identity.AccountID, WorkspaceID: identity.WorkspaceID}, nil
}
