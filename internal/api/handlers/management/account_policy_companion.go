package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

// GetAccountPolicyCapabilities advertises only the optional companion contract.
func (h *Handler) GetAccountPolicyCapabilities(c *gin.Context) {
	if service := h.policyService(c); service != nil {
		settings := service.Settings()
		c.JSON(http.StatusOK, gin.H{"contract_version": 1, "confirmation_lifetime_seconds": 120, "caller_idempotency": true, "snapshot_precondition": true, "observations_enabled": settings.Enabled || settings.ObservationsEnabled, "reset_writes_enabled": settings.Enabled && !settings.ReadOnly})
	}
}

func (h *Handler) RedeemAccountPolicyConfirmedReset(c *gin.Context) {
	var request accountpolicy.ConfirmedResetRequest
	if !decodePolicyJSON(c, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		policyFailure(c, http.StatusBadRequest, "invalid_request", "Complete confirmation evidence is required.")
		return
	}
	service := h.policyService(c)
	if service == nil {
		return
	}
	op, err := service.RedeemConfirmed(c.Request.Context(), request)
	if err != nil {
		policyError(c, err)
		return
	}
	if op.Error != "" {
		op.Error = "Operation requires reconciliation."
	}
	op.Before = sanitizedSnapshotCopy(op.Before)
	op.After = sanitizedSnapshotCopy(op.After)
	c.JSON(http.StatusOK, gin.H{"operation": op})
}
