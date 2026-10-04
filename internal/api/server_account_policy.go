package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyui"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
)

// WithAccountPolicy attaches the lifecycle-owned optional account policy service.
func WithAccountPolicy(service *accountpolicy.Service) ServerOption {
	return func(cfg *serverOptionConfig) { cfg.accountPolicy = service }
}

// WithAccountPolicySettingsValidator enforces runtime scheduler ownership on settings updates.
func WithAccountPolicySettingsValidator(validate func(accountpolicy.Settings) error) ServerOption {
	return func(cfg *serverOptionConfig) { cfg.accountPolicyValidator = validate }
}

// WithAccountPolicyUsage supplies the lifecycle-owned optional durable sink.
func WithAccountPolicyUsage(source func() *accountpolicyusage.Sink) ServerOption {
	return func(cfg *serverOptionConfig) { cfg.accountPolicyUsage = source }
}

// Policy routes reuse upstream availability and authentication without changing
// the middleware or error contract of unrelated management endpoints.
func (s *Server) registerAccountPolicyManagementRoutes() {
	policy := s.engine.Group("/v8/management/account-policy")
	policy.Use(s.managementAvailabilityMiddleware(), s.mgmt.AccountPolicyMiddleware(), func(c *gin.Context) {
		c.Set(management.ConfigV8ContextKey, true)
	})
	policy.GET("/accounts", s.mgmt.GetAccountPolicyAccounts)
	policy.GET("/capabilities", s.mgmt.GetAccountPolicyCapabilities)
	policy.GET("/dashboard", s.mgmt.GetAccountPolicyDashboard)
	policy.GET("/decisions", s.mgmt.GetAccountPolicyDecisions)
	policy.GET("/resets", s.mgmt.GetAccountPolicyResets)
	policy.PATCH("/settings", s.mgmt.PatchAccountPolicySettings)
	policy.POST("/refresh", s.mgmt.RefreshAccountPolicy)
	policy.POST("/resets/redeem", s.mgmt.RedeemAccountPolicyReset)
	policy.POST("/resets/redeem-confirmed", s.mgmt.RedeemAccountPolicyConfirmedReset)
	policy.POST("/resets/schedule", s.mgmt.ScheduleAccountPolicyReset)
	policy.DELETE("/resets/schedule/:schedule_id", s.mgmt.CancelAccountPolicySchedule)
	policy.GET("/diagnostics", s.mgmt.GetAccountPolicyDiagnostics)
}

func (s *Server) registerAccountPolicyPanelRoutes() {
	for _, path := range []string{"/account-policy.html", "/account-policy.js", "/account-policy-dashboard.js", "/account-policy-icon.svg", "/account-policy.css"} {
		s.engine.GET(path, s.serveAccountPolicyPanel)
	}
}

func (s *Server) serveAccountPolicyPanel(c *gin.Context) {
	cfg := s.getConfig()
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	accountpolicyui.Handler().ServeHTTP(c.Writer, c.Request)
}
