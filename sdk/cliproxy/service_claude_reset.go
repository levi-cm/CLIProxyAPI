package cliproxy

import (
	"context"
	"net/http"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudereset"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// startClaudeResets starts automatic Claude reset redemption. It reads reset
// inventory always, but claims only while this instance is the account-policy
// writer, the policy is enabled and writable, and claude_resets is not off.
func (s *Service) startClaudeResets(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.accountPolicy == nil {
		return
	}
	policy := s.accountPolicy
	s.coreManager.StartClaudeResetController(ctx, claudeResetClient{service: s}, coreauth.ClaudeResetOptions{
		Gate: func() (bool, string) {
			settings := policy.Settings()
			switch {
			case settings.ClaudeResets == "off":
				return false, "off"
			case !settings.Enabled:
				return false, "policy_disabled"
			case settings.ReadOnly:
				return false, "read_only"
			case !policy.Writer():
				return false, "not_writer"
			}
			return true, ""
		},
		Lock:     policy.LockRedemption,
		StateDir: policy.StateDir,
		Held:     policy.Held,
		Forced:   policy.ForcedAccount,
		Revision: policy.RoutingRevision,
	})
}

type claudeResetClient struct {
	service *Service
}

func (c claudeResetClient) client(a *coreauth.Auth) *claudeauth.ClaudeAuth {
	return claudeauth.NewClaudeAuthWithProxyURL(c.service.currentConfig(), a.ProxyURL)
}

func (c claudeResetClient) Status(ctx context.Context, a *coreauth.Auth, accessToken string) (claudereset.Observation, http.Header, error) {
	usage, err := c.client(a).FetchOAuthUsageWithResets(ctx, accessToken)
	if err != nil {
		return claudereset.Observation{}, nil, err
	}
	return claudeResetObservation(usage, time.Now()), usage.RateLimitHeaders(), nil
}

func (c claudeResetClient) Identity(ctx context.Context, a *coreauth.Auth, accessToken string) (string, string, error) {
	profile, err := c.client(a).OAuthProfileForResets(ctx, accessToken)
	if err != nil {
		return "", "", err
	}
	return profile.Account.UUID, profile.Organization.UUID, nil
}

func (c claudeResetClient) Claim(ctx context.Context, a *coreauth.Auth, accessToken, organizationUUID, grantID, requestID string) (string, error) {
	return c.client(a).ClaimRateLimitReset(ctx, accessToken, organizationUUID, grantID, requestID)
}
