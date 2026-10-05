package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyclient"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// Client usage uses inference authentication, never the management key. It fails
// closed even when upstream permits anonymous inference. No existing route or
// response-header passthrough default is changed.
func (s *Server) registerAccountPolicyClientRoutes(service *accountpolicy.Service) {
	group := s.engine.Group("/v1/account-policy")
	group.Use(AuthMiddleware(s.accessManager), func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if value, ok := c.Get("userApiKey"); !ok || value == nil || strings.TrimSpace(c.GetString("userApiKey")) == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "unauthorized", "message": "Authenticated inference access is required."}})
		}
	})
	group.GET("/usage", func(c *gin.Context) {
		report, err := s.accountPolicyClientReport(service, c.GetString("userApiKey"), c.Query("session_id"))
		if err != nil {
			status := http.StatusServiceUnavailable
			if e, ok := err.(*accountpolicy.Error); ok && e.Code == "invalid_session" {
				status = http.StatusBadRequest
			}
			c.JSON(status, gin.H{"error": gin.H{"code": "unavailable", "message": "Usage requires a valid session ID and an available account policy service."}})
			return
		}
		c.JSON(http.StatusOK, report)
	})
	group.POST("/mcp", func(c *gin.Context) {
		accountpolicyclient.ServeMCP(c.Writer, c.Request, func(id string) (accountpolicyclient.Report, error) {
			return s.accountPolicyClientReport(service, c.GetString("userApiKey"), id)
		})
	})
}

func (s *Server) accountPolicyClientReport(service *accountpolicy.Service, principal, threadID string) (accountpolicyclient.Report, error) {
	if service == nil || s.handlers.AuthManager == nil {
		return accountpolicyclient.Report{}, &accountpolicy.Error{Code: "unavailable"}
	}
	if !accountpolicyclient.ValidSessionID(threadID) {
		return accountpolicyclient.Report{}, &accountpolicy.Error{Code: "invalid_session"}
	}
	now := time.Now().UTC()
	settings := service.Settings()
	report := accountpolicyclient.Report{SchemaVersion: 1, SessionID: threadID, SampledAt: now, TimeZone: settings.TimeZone, State: "no_selection", ActiveAccounts: []accountpolicyclient.Account{}}
	active, last, completedAt := s.handlers.AuthManager.PolicyClientRuntimeSnapshot(session.CallerScope(principal), "codex:"+threadID, now)
	account := func(a auth.PolicyClientAccount) *accountpolicyclient.Account {
		if a.Provider != "codex" {
			return nil
		}
		value := &accountpolicyclient.Account{CredentialID: a.CredentialID, Provider: a.Provider, Label: a.Label, ActiveRequests: a.ActiveRequests}
		snapshot, found := service.Snapshot(a.CredentialID)
		identity := a.Identity
		if !found || identity == nil || snapshot.Identity.CredentialID != identity.CredentialID || snapshot.Identity.Provider != identity.Provider || snapshot.Identity.AccountID != identity.AccountID || snapshot.Identity.WorkspaceID != identity.WorkspaceID {
			snapshot = accountpolicy.Snapshot{}
		}
		value.Weekly = accountpolicyclient.WeeklyUsage(snapshot, settings.FreshnessSeconds, now)
		return value
	}
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if value := account(active[id]); value != nil {
			report.ActiveAccounts = append(report.ActiveAccounts, *value)
		}
	}
	if len(report.ActiveAccounts) > 0 {
		report.State = "serving"
	}
	if last != nil {
		report.LastSuccessfulAccount = account(*last)
		if report.LastSuccessfulAccount != nil {
			report.LastCompletedAt = &completedAt
			if report.State != "serving" {
				report.State = "idle"
			}
		}
	}
	return report, nil
}
