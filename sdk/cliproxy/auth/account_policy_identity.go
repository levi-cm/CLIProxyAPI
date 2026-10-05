package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// PolicyAccountIdentity is shared by provider discovery and client quota reads;
// token claims are decoded only for ownership validation, never returned.
func PolicyAccountIdentity(a *Auth) (accountpolicy.Identity, error) {
	identity := accountpolicy.Identity{}
	if a == nil || a.Provider != "codex" || a.ID == "" {
		return identity, &accountpolicy.Error{Code: "invalid_account", Message: "Codex credential identity is unavailable"}
	}
	account := authMetadataString(a, "account_id")
	for _, tokenKey := range []string{"id_token", "access_token"} {
		token := authMetadataString(a, tokenKey)
		if token == "" || strings.Count(token, ".") != 2 {
			continue
		}
		claims, err := codexauth.ParseJWTToken(token)
		if err != nil {
			return identity, &accountpolicy.Error{Code: "identity_mismatch", Message: "Codex credential claims are invalid"}
		}
		claimAccount := strings.TrimSpace(claims.GetAccountID())
		if claimAccount == "" {
			continue
		}
		if account != "" && account != claimAccount {
			return identity, &accountpolicy.Error{Code: "identity_mismatch", Message: "Codex account metadata conflicts with credential claims"}
		}
		account = claimAccount
	}
	workspace := authMetadataString(a, "workspace_id")
	if account == "" || workspace != "" && workspace != account {
		return identity, &accountpolicy.Error{Code: "identity_mismatch", Message: "Codex account and workspace mapping is unsupported"}
	}
	return accountpolicy.Identity{CredentialID: a.ID, AccountID: account, WorkspaceID: account, Provider: "codex", Alias: a.Label, Generation: a.Generation}, nil
}
