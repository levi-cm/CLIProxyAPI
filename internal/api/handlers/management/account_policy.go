package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
)

func (h *Handler) SetAccountPolicy(service *accountpolicy.Service) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.accountPolicy = service
	h.mu.Unlock()
}

// SetAccountPolicySettingsValidator preserves lifecycle ownership constraints
// when an operator enables or changes the policy through the management API.
func (h *Handler) SetAccountPolicySettingsValidator(validate func(accountpolicy.Settings) error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.accountPolicyValidator = validate
	h.mu.Unlock()
}

// AccountPolicyMiddleware uses the existing management authentication and its
// remote-access restrictions, with the policy API's stable error envelope.
func (h *Handler) AccountPolicyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("X-Management-Key")
		if authorization := c.GetHeader("Authorization"); authorization != "" {
			parts := strings.SplitN(authorization, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				key = parts[1]
			} else {
				key = authorization
			}
		}
		ip := c.ClientIP()
		allowed, status, _ := h.AuthenticateManagementKey(ip, ip == "127.0.0.1" || ip == "::1", key)
		if !allowed {
			code := "unauthorized"
			if status == http.StatusForbidden {
				code = "forbidden"
			}
			policyFailure(c, status, code, "Management access denied.")
			return
		}
		c.Next()
	}
}

func policyFailure(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func policyError(c *gin.Context, err error) {
	var e *accountpolicy.Error
	if errors.As(err, &e) {
		status, message := http.StatusConflict, "Account policy operation cannot proceed."
		switch e.Code {
		case "invalid_settings", "invalid_schedule":
			status, message = http.StatusBadRequest, "Invalid account policy settings or schedule."
		case "unknown_account", "unknown_credit", "unknown_schedule":
			status, message = http.StatusNotFound, "The selected account, credit, or schedule is unavailable."
		case "disabled":
			status, message = http.StatusServiceUnavailable, "Account policy is disabled."
		case "read_only":
			status, message = http.StatusForbidden, "This account policy instance is read only."
		case "persistence":
			status, message = http.StatusInternalServerError, "Account policy state could not be saved."
		case "provider_error", "identity_mismatch":
			status, message = http.StatusBadGateway, "Provider evidence could not be verified."
		}
		policyFailure(c, status, e.Code, message)
		return
	}
	policyFailure(c, http.StatusInternalServerError, "provider_error", "Account policy operation failed.")
}

func (h *Handler) policyService(c *gin.Context) *accountpolicy.Service {
	h.mu.Lock()
	service := h.accountPolicy
	h.mu.Unlock()
	if service == nil {
		policyFailure(c, http.StatusServiceUnavailable, "unavailable", "Account policy service is unavailable.")
	}
	return service
}

func decodePolicyJSON(c *gin.Context, destination any) bool {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		policyFailure(c, http.StatusBadRequest, "invalid_request", "Expected a valid JSON object with supported fields.")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		policyFailure(c, http.StatusBadRequest, "invalid_request", "Expected exactly one JSON object.")
		return false
	}
	return true
}

func sanitizedAccounts(service *accountpolicy.Service) []accountpolicy.Snapshot {
	accounts := append([]accountpolicy.Snapshot{}, service.Accounts()...)
	if accounts == nil {
		accounts = []accountpolicy.Snapshot{}
	}
	for i := range accounts {
		sanitizeSnapshot(&accounts[i])
	}
	return accounts
}
func sanitizeSnapshot(snapshot *accountpolicy.Snapshot) {
	if snapshot != nil && snapshot.LastError != "" {
		snapshot.LastError = "Provider observation failed."
	}
}
func sanitizedOperations(service *accountpolicy.Service) []accountpolicy.Operation {
	operations := append([]accountpolicy.Operation{}, service.Operations()...)
	if operations == nil {
		operations = []accountpolicy.Operation{}
	}
	for i := range operations {
		if operations[i].Error != "" {
			operations[i].Error = "Operation failed."
		}
		operations[i].Before = sanitizedSnapshotCopy(operations[i].Before)
		operations[i].After = sanitizedSnapshotCopy(operations[i].After)
	}
	return operations
}

func sanitizedSnapshotCopy(snapshot *accountpolicy.Snapshot) *accountpolicy.Snapshot {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	sanitizeSnapshot(&copy)
	return &copy
}
func policyDecisions(service *accountpolicy.Service) []accountpolicy.Decision {
	values := service.Decisions()
	if values == nil {
		return []accountpolicy.Decision{}
	}
	return values
}
func policySchedules(service *accountpolicy.Service) []accountpolicy.Schedule {
	values := service.Schedules()
	if values == nil {
		return []accountpolicy.Schedule{}
	}
	return values
}

func (h *Handler) GetAccountPolicyAccounts(c *gin.Context) {
	if service := h.policyService(c); service != nil {
		c.JSON(http.StatusOK, gin.H{"settings": service.Settings(), "accounts": sanitizedAccounts(service)})
	}
}
func (h *Handler) GetAccountPolicyDecisions(c *gin.Context) {
	if service := h.policyService(c); service != nil {
		c.JSON(http.StatusOK, gin.H{"decisions": policyDecisions(service)})
	}
}
func (h *Handler) GetAccountPolicyResets(c *gin.Context) {
	if service := h.policyService(c); service != nil {
		c.JSON(http.StatusOK, gin.H{"operations": sanitizedOperations(service), "schedules": policySchedules(service)})
	}
}
func (h *Handler) GetAccountPolicyDiagnostics(c *gin.Context) {
	if service := h.policyService(c); service != nil {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"settings": service.Settings(), "accounts": sanitizedAccounts(service), "decisions": policyDecisions(service), "operations": sanitizedOperations(service), "schedules": policySchedules(service)})
	}
}

func (h *Handler) PatchAccountPolicySettings(c *gin.Context) {
	service := h.policyService(c)
	if service == nil {
		return
	}
	var patch map[string]json.RawMessage
	if !decodePolicyJSON(c, &patch) {
		return
	}
	if patch == nil {
		policyFailure(c, http.StatusBadRequest, "invalid_settings", "Settings must be a JSON object.")
		return
	}
	for _, raw := range patch {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			policyFailure(c, http.StatusBadRequest, "invalid_settings", "Settings fields cannot be null.")
			return
		}
	}
	err := service.MergeSettings(func(current accountpolicy.Settings) (accountpolicy.Settings, error) {
		encoded, _ := json.Marshal(current)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(encoded, &fields)
		for name, value := range patch {
			if name == "accounts" {
				var controls map[string]map[string]json.RawMessage
				if err := json.Unmarshal(value, &controls); err != nil || controls == nil {
					return current, &accountpolicy.Error{Code: "invalid_settings"}
				}
				if current.Accounts == nil {
					current.Accounts = map[string]accountpolicy.AccountControl{}
				}
				for id, changes := range controls {
					if strings.TrimSpace(id) == "" || changes == nil {
						return current, &accountpolicy.Error{Code: "invalid_settings"}
					}
					old := current.Accounts[id]
					raw, _ := json.Marshal(old)
					var merged map[string]json.RawMessage
					_ = json.Unmarshal(raw, &merged)
					for key, change := range changes {
						if bytes.Equal(bytes.TrimSpace(change), []byte("null")) {
							return current, &accountpolicy.Error{Code: "invalid_settings"}
						}
						merged[key] = change
					}
					raw, _ = json.Marshal(merged)
					decoder := json.NewDecoder(bytes.NewReader(raw))
					decoder.DisallowUnknownFields()
					if err := decoder.Decode(&old); err != nil {
						return current, &accountpolicy.Error{Code: "invalid_settings"}
					}
					current.Accounts[id] = old
				}
				fields[name], _ = json.Marshal(current.Accounts)
			} else {
				fields[name] = value
			}
		}
		encoded, _ = json.Marshal(fields)
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		var next accountpolicy.Settings
		if err := decoder.Decode(&next); err != nil {
			return current, &accountpolicy.Error{Code: "invalid_settings"}
		}
		h.mu.Lock()
		validate := h.accountPolicyValidator
		h.mu.Unlock()
		if validate != nil {
			if errValidate := validate(next); errValidate != nil {
				return current, errValidate
			}
		}
		return next, nil
	})
	if err != nil {
		policyError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": service.Settings()})
}

func (h *Handler) RefreshAccountPolicy(c *gin.Context) {
	service := h.policyService(c)
	if service == nil {
		return
	}
	var request struct {
		CredentialID string `json:"credential_id"`
	}
	if !decodePolicyJSON(c, &request) {
		return
	}
	if err := service.Refresh(c.Request.Context(), strings.TrimSpace(request.CredentialID)); err != nil {
		policyError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": sanitizedAccounts(service)})
}

type policyCreditRequest struct {
	CredentialID string `json:"credential_id"`
	CreditID     string `json:"credit_id"`
	At           string `json:"at,omitempty"`
}

func validPolicySelection(c *gin.Context, id, credit string) bool {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(credit) == "" {
		policyFailure(c, http.StatusBadRequest, "invalid_request", "Both credential_id and credit_id are required.")
		return false
	}
	return true
}
func (h *Handler) RedeemAccountPolicyReset(c *gin.Context) {
	service := h.policyService(c)
	if service == nil {
		return
	}
	var request struct {
		CredentialID string `json:"credential_id"`
		CreditID     string `json:"credit_id"`
	}
	if !decodePolicyJSON(c, &request) || !validPolicySelection(c, request.CredentialID, request.CreditID) {
		return
	}
	operation, err := service.Redeem(c.Request.Context(), request.CredentialID, request.CreditID)
	if err != nil {
		policyError(c, err)
		return
	}
	if operation.Error != "" {
		operation.Error = "Operation failed."
	}
	operation.Before = sanitizedSnapshotCopy(operation.Before)
	operation.After = sanitizedSnapshotCopy(operation.After)
	c.JSON(http.StatusOK, gin.H{"operation": operation})
}
func (h *Handler) ScheduleAccountPolicyReset(c *gin.Context) {
	service := h.policyService(c)
	if service == nil {
		return
	}
	var request policyCreditRequest
	if !decodePolicyJSON(c, &request) || !validPolicySelection(c, request.CredentialID, request.CreditID) {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, request.At)
	if err != nil {
		policyFailure(c, http.StatusBadRequest, "invalid_schedule", "Schedule at must be RFC3339 with an explicit offset.")
		return
	}
	schedule, errSchedule := service.Schedule(request.CredentialID, request.CreditID, at.UTC())
	if errSchedule != nil {
		policyError(c, errSchedule)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedule": schedule})
}
func (h *Handler) CancelAccountPolicySchedule(c *gin.Context) {
	if service := h.policyService(c); service != nil {
		if err := service.CancelSchedule(c.Param("schedule_id")); err != nil {
			policyError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"cancelled": true})
	}
}
