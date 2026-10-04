package management

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type dashboardActivityAccount struct {
	CredentialID   string          `json:"credential_id"`
	Alias          string          `json:"alias"`
	Provider       string          `json:"provider"`
	ActiveRequests int             `json:"active_requests"`
	ActiveBindings int             `json:"active_bindings"`
	Transport      string          `json:"transport"`
	LastSelectedAt *time.Time      `json:"last_selected_at"`
	Status         coreauth.Status `json:"status"`
	Disabled       bool            `json:"disabled"`
}

var dashboardSensitiveAlias = regexp.MustCompile(`(?i)(bearer\s|sk-|eyJ[a-zA-Z0-9_-]+\.|access[_ -]?token|refresh[_ -]?token|api[_ -]?key|https?://)`)

func dashboardAliasContainsSecret(label, secret string) bool {
	if secret == "" {
		return false
	}
	if strings.Contains(label, secret) {
		return true
	}
	// Authorization metadata can include a scheme; cookies can include names.
	// Compare credential values as well as their complete transport header.
	for _, fragment := range strings.Fields(secret) {
		if fragment != "" && strings.Contains(label, fragment) {
			return true
		}
	}
	for _, cookie := range strings.Split(secret, ";") {
		if _, value, ok := strings.Cut(cookie, "="); ok && strings.TrimSpace(value) != "" && strings.Contains(label, strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

// dashboardAlias permits a bounded display label but never substitutes token,
// file, account, workspace, or email metadata when a label is unavailable.
func dashboardAlias(a *coreauth.Auth) string {
	label := strings.TrimSpace(a.Label)
	if len(label) > 160 || dashboardSensitiveAlias.MatchString(label) || strings.ContainsFunc(label, unicode.IsControl) {
		return "Account"
	}
	for key, value := range a.Metadata {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "key") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") {
			if secret, ok := value.(string); ok && dashboardAliasContainsSecret(label, secret) {
				return "Account"
			}
		}
	}
	for key, secret := range a.Attributes {
		lower := strings.ToLower(key)
		if (strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "key") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie")) && dashboardAliasContainsSecret(label, secret) {
			return "Account"
		}
	}
	if label == "" {
		return "Account"
	}
	return label
}

func (h *Handler) SetAccountPolicyUsage(source func() *accountpolicyusage.Sink) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.accountPolicyUsage = source
	h.mu.Unlock()
}

// GetAccountPolicyDashboard samples only local runtime leases and retained
// usage records. It does not refresh provider evidence or drain the usage queue.
func (h *Handler) GetAccountPolicyDashboard(c *gin.Context) {
	window := 24 * time.Hour
	switch c.DefaultQuery("range", "24h") {
	case "1h":
		window = time.Hour
	case "24h":
	case "7d":
		window = 7 * 24 * time.Hour
	default:
		policyFailure(c, http.StatusBadRequest, "invalid_range", "Range must be 1h, 24h, or 7d.")
		return
	}
	now := time.Now().UTC()
	h.mu.Lock()
	manager, source := h.authManager, h.accountPolicyUsage
	h.mu.Unlock()
	accounts := []dashboardActivityAccount{}
	activityAvailable := manager != nil
	if manager != nil {
		registered := manager.List()
		ids := make([]string, 0, len(registered))
		for _, a := range registered {
			if a != nil && a.ID != "" && a.Provider != "" {
				ids = append(ids, a.ID)
			}
		}
		runtimes, available := manager.PolicyRuntimeSnapshot(ids)
		activityAvailable = available
		for _, a := range registered {
			if a == nil || a.ID == "" || a.Provider == "" {
				continue
			}
			runtime := runtimes[a.ID]
			status := a.Status
			switch status {
			case coreauth.StatusActive, coreauth.StatusPending, coreauth.StatusRefreshing, coreauth.StatusError, coreauth.StatusDisabled:
			default:
				status = coreauth.StatusUnknown
			}
			accounts = append(accounts, dashboardActivityAccount{CredentialID: a.ID, Alias: dashboardAlias(a), Provider: a.Provider, ActiveRequests: runtime.ActiveRequests, ActiveBindings: runtime.ActiveBindings, Transport: runtime.Transport, LastSelectedAt: runtime.LastSelectedAt, Status: status, Disabled: a.Disabled || a.Status == coreauth.StatusDisabled})
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].CredentialID < accounts[j].CredentialID })
	var sink *accountpolicyusage.Sink
	if source != nil {
		sink = source()
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"sampled_at": now, "activity": gin.H{"available": activityAvailable, "accounts": accounts}, "usage": sink.Dashboard(now, window)})
}
