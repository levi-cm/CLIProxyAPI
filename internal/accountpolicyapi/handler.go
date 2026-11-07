// Package accountpolicyapi shares global integration logic between inference and
// management authentication. Authentication belongs to each mounting router.
package accountpolicyapi

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyledger"
)

type Handler struct {
	Service *accountpolicy.Service
	History func() *accountpolicyledger.Archive
}

func (h *Handler) Register(g *gin.RouterGroup, accounts bool) {
	if accounts {
		g.GET("/accounts", h.accounts)
	}
	g.GET("/routing-override", h.override)
	g.PUT("/routing-override", h.replace)
	g.DELETE("/routing-override", h.cancel)
	g.GET("/allowance-history", h.allowance)
	g.GET("/usage-history/requests", h.requests)
	g.GET("/usage-history/summary", h.summary)
	g.GET("/usage-history/export", h.export)
	g.GET("/usage-history/status", h.status)
}
func failure(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
func (h *Handler) service(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	if h == nil || h.Service == nil {
		failure(c, 503, "unavailable", "Account policy service unavailable.")
		return false
	}
	return true
}
func (h *Handler) archive(c *gin.Context) *accountpolicyledger.Archive {
	c.Header("Cache-Control", "no-store")
	if h == nil || h.History == nil || h.History() == nil {
		failure(c, 503, "unavailable", "Usage history unavailable.")
		return nil
	}
	return h.History()
}
func policyFailure(c *gin.Context, err error) {
	var e *accountpolicy.Error
	if errors.As(err, &e) {
		status := 409
		switch e.Code {
		case "invalid_duration", "invalid_request", "invalid_split", "invalid_pressure":
			status = 400
		case "unknown_account":
			status = 404
		case "disabled", "unavailable":
			status = 503
		case "read_only":
			status = 403
		case "persistence":
			status = 500
		}
		failure(c, status, e.Code, e.Message)
		return
	}
	failure(c, 500, "unavailable", "Operation failed.")
}
func (h *Handler) override(c *gin.Context) {
	if !h.service(c) {
		return
	}
	if err := h.Service.MaintainOverride(); err != nil {
		policyFailure(c, err)
		return
	}
	c.JSON(200, h.Service.RoutingOverride())
}
func (h *Handler) replace(c *gin.Context) {
	if !h.service(c) {
		return
	}
	var body struct {
		Mode             string                       `json:"mode"`
		Accounts         []accountpolicy.RoutingShare `json:"accounts"`
		CredentialID     string                       `json:"credential_id"`
		DurationHours    int                          `json:"duration_hours"`
		ExpectedRevision *uint64                      `json:"expected_revision"`
	}
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	d.DisallowUnknownFields()
	var extra any
	if err := d.Decode(&body); err != nil || body.ExpectedRevision == nil || d.Decode(&extra) != io.EOF {
		failure(c, 400, "invalid_request", "Provide supported routing fields, whole duration_hours and expected_revision.")
		return
	}
	var o accountpolicy.RoutingOverride
	var err error
	switch body.Mode {
	case "", "single":
		if strings.TrimSpace(body.CredentialID) == "" || len(body.Accounts) != 0 {
			failure(c, 400, "invalid_request", "Single routing requires credential_id and no account shares.")
			return
		}
		o, err = h.Service.ReplaceOverride(body.CredentialID, body.DurationHours, *body.ExpectedRevision)
	case "pressure":
		if body.CredentialID != "" {
			failure(c, 400, "invalid_request", "Pressure routing accepts accounts instead of credential_id.")
			return
		}
		o, err = h.Service.ReplacePressureOverride(body.Accounts, body.DurationHours, *body.ExpectedRevision)
	case "split":
		if body.CredentialID != "" {
			failure(c, 400, "invalid_request", "Percentage routing accepts accounts instead of credential_id.")
			return
		}
		o, err = h.Service.ReplaceSplitOverride(body.Accounts, body.DurationHours, *body.ExpectedRevision)
	default:
		failure(c, 400, "invalid_request", "Routing mode must be single, pressure or split.")
		return
	}
	if err != nil {
		policyFailure(c, err)
		return
	}
	c.JSON(200, o)
}
func (h *Handler) cancel(c *gin.Context) {
	if !h.service(c) {
		return
	}
	revision, err := strconv.ParseUint(c.Query("expected_revision"), 10, 64)
	if err != nil {
		failure(c, 400, "invalid_request", "expected_revision is required.")
		return
	}
	o, err := h.Service.CancelOverride(revision)
	if err != nil {
		policyFailure(c, err)
		return
	}
	c.JSON(200, o)
}
func (h *Handler) accounts(c *gin.Context) {
	if !h.service(c) {
		return
	}
	settings := h.Service.Settings()
	out := []gin.H{}
	for _, s := range h.Service.Accounts() {
		evaluation := accountpolicy.Evaluate(s, "", settings, time.Now().UTC())
		buckets := []accountpolicy.Bucket{}
		for _, b := range s.Buckets {
			b.Source = ""
			buckets = append(buckets, b)
		}
		out = append(out, gin.H{"credential_id": s.Identity.CredentialID, "provider": s.Identity.Provider, "label": "Account", "eligible": evaluation.Eligible, "evidence_known": evaluation.Known, "eligibility_reason": evaluation.Reason, "serving_on_credits": evaluation.Credits, "observed_at": s.ObservedAt, "buckets": buckets, "temporary_routing_supported": s.Identity.Provider == "codex"})
	}
	c.JSON(200, gin.H{"schema_version": 1, "sampled_at": time.Now().UTC(), "accounts": out, "capabilities": gin.H{"global_override": true, "pressure_spreading": true, "automatic_spreading": true, "percentage_split": false, "legacy_split_membership": true, "split_min_percent": 1, "split_max_percent": 99, "policy_enabled": settings.Enabled, "duration_min_hours": 1, "duration_max_hours": 24, "account_bound_conversations_retain_owner": true}})
}
func parseQuery(c *gin.Context) (accountpolicyledger.Query, error) {
	q := accountpolicyledger.Query{Provider: c.Query("provider"), Model: c.Query("model"), CredentialID: c.Query("credential_id"), Cursor: c.Query("cursor"), Limit: 100}
	var err error
	if s := c.Query("from"); s != "" {
		q.From, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return q, err
		}
	}
	if s := c.Query("to"); s != "" {
		q.To, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return q, err
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.To.After(q.From) {
		return q, errors.New("invalid range")
	}
	if s := c.Query("limit"); s != "" {
		q.Limit, err = strconv.Atoi(s)
		if err != nil || q.Limit < 1 || q.Limit > 500 {
			return q, errors.New("invalid limit")
		}
	}
	return q, nil
}
func (h *Handler) requests(c *gin.Context) {
	a := h.archive(c)
	if a == nil {
		return
	}
	q, err := parseQuery(c)
	if err != nil {
		failure(c, 400, "invalid_query", "Use UTC timestamps, valid filters and a limit of 1–500.")
		return
	}
	p, err := a.RequestsContext(c.Request.Context(), q)
	if err != nil {
		failure(c, 400, "history_query_failed", "Invalid cursor or unavailable history; check collection status.")
		return
	}
	c.JSON(200, p)
}
func (h *Handler) summary(c *gin.Context) {
	a := h.archive(c)
	if a == nil {
		return
	}
	q, err := parseQuery(c)
	if err != nil {
		failure(c, 400, "invalid_query", "Invalid range or filters.")
		return
	}
	group := c.DefaultQuery("group", "day")
	if group != "day" && group != "month" && group != "year" {
		failure(c, 400, "invalid_query", "group must be day, month or year.")
		return
	}
	// Stream one calendar period at a time so multi-year queries never accumulate
	// their complete result in memory. Groups are additionally split by provider,
	// actual execution model and credential.
	if q.From.IsZero() {
		q.From = a.Status().CoverageStart
	}
	if q.To.IsZero() {
		q.To = time.Now().UTC()
	}
	if q.From.IsZero() {
		c.JSON(200, gin.H{"schema_version": 1, "groups": []any{}})
		return
	}
	c.Header("Content-Type", "application/json")
	c.Status(200)
	_, _ = io.WriteString(c.Writer, `{"schema_version":1,"groups":[`)
	first := true
	for from := q.From; from.Before(q.To); {
		end := from.UTC()
		switch group {
		case "year":
			end = time.Date(end.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
		case "month":
			end = time.Date(end.Year(), end.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		default:
			end = time.Date(end.Year(), end.Month(), end.Day()+1, 0, 0, 0, 0, time.UTC)
		}
		if end.After(q.To) {
			end = q.To
		}
		part := q
		part.From = from
		part.To = end
		totals, e := a.SummaryContext(c.Request.Context(), part, group)
		if e != nil {
			return
		}
		for _, t := range totals {
			if !first {
				_, _ = io.WriteString(c.Writer, ",")
			}
			first = false
			if e = json.NewEncoder(c.Writer).Encode(t); e != nil {
				return
			}
		}
		from = end
		if c.Request.Context().Err() != nil {
			return
		}
	}
	_, _ = io.WriteString(c.Writer, ` ]}`)
}
func (h *Handler) export(c *gin.Context) {
	a := h.archive(c)
	if a == nil {
		return
	}
	q, err := parseQuery(c)
	format := c.DefaultQuery("format", "ndjson")
	if err != nil || (format != "ndjson" && format != "csv") {
		failure(c, 400, "invalid_query", "Use csv or ndjson and valid filters.")
		return
	}
	c.Header("Content-Disposition", `attachment; filename="usage-history.`+format+`"`)
	if format == "ndjson" {
		c.Header("Content-Type", "application/x-ndjson")
		err = a.ExportContext(c.Request.Context(), q, func(r accountpolicyledger.Request) error {
			if e := c.Request.Context().Err(); e != nil {
				return e
			}
			return json.NewEncoder(c.Writer).Encode(r)
		})
	} else {
		c.Header("Content-Type", "text/csv; charset=utf-8")
		w := csv.NewWriter(c.Writer)
		_ = w.Write([]string{"execution_uuid", "parent_execution_uuid", "trace_id", "timestamp_utc", "provider", "credential_id", "requested_model", "execution_model", "response_model", "outcome", "status_code", "duration_ms", "stream", "generate", "auth_category", "requested_service_tier", "response_service_tier", "input_tokens", "uncached_input_tokens", "cache_read_tokens", "cache_write_tokens", "cache_write_5m_tokens", "cache_write_1h_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "unclassified_tokens", "cached_input_percent", "accounting_quality", "measurements_json"})
		err = a.ExportContext(c.Request.Context(), q, func(r accountpolicyledger.Request) error {
			if e := c.Request.Context().Err(); e != nil {
				return e
			}
			percent := ""
			if r.CachedInputPercent != nil {
				percent = strconv.FormatFloat(*r.CachedInputPercent, 'f', -1, 64)
			}
			presence, _ := json.Marshal(r.Measurements)
			return w.Write([]string{r.RequestID, r.ParentExecutionID, r.TraceID, r.RequestedAt.Format(time.RFC3339Nano), r.Provider, r.CredentialID, r.RequestedModel, r.ExecutionModel, r.ResponseModel, r.Outcome, strconv.Itoa(r.StatusCode), fmt.Sprint(r.DurationMS), fmt.Sprint(r.Stream), fmt.Sprint(r.Generate), r.AuthCategory, r.RequestedServiceTier, r.ResponseServiceTier, fmt.Sprint(r.Tokens.Input.TotalTokens), fmt.Sprint(r.Tokens.Input.UncachedTokens), fmt.Sprint(r.Tokens.Input.CacheReadTokens), fmt.Sprint(r.Tokens.Input.CacheWriteTokens), optionalInt(r.CacheWrite5m), optionalInt(r.CacheWrite1h), fmt.Sprint(r.Tokens.Output.TotalTokens), fmt.Sprint(r.Tokens.Output.ReasoningTokens), fmt.Sprint(r.Tokens.TotalTokens), fmt.Sprint(r.Tokens.UnclassifiedTokens), percent, string(r.Tokens.Quality), string(presence)})
		})
		w.Flush()
		if err == nil {
			err = w.Error()
		}
	}
	if err != nil && !c.Writer.Written() {
		failure(c, 500, "history_unavailable", "History export failed.")
	}
}
func optionalInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}
func (h *Handler) status(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var a *accountpolicyledger.Archive
	if h != nil && h.History != nil {
		a = h.History()
	}
	c.JSON(200, a.Status())
}
func (h *Handler) allowance(c *gin.Context) {
	a := h.archive(c)
	if a == nil {
		return
	}
	q, err := parseQuery(c)
	duration := int64(604800)
	switch c.DefaultQuery("window", "weekly") {
	case "weekly":
	case "5h":
		duration = 18000
	default:
		err = errors.New("invalid window")
	}
	if err != nil || q.CredentialID == "" || q.From.IsZero() || q.To.IsZero() || q.To.Sub(q.From) > 31*24*time.Hour || !q.To.After(q.From) {
		failure(c, 400, "invalid_query", "One credential, weekly or 5h window and a range up to 31 days required.")
		return
	}
	c.Header("Content-Type", "application/json")
	c.Status(200)
	_, _ = io.WriteString(c.Writer, `{"schema_version":1,"observations":[`)
	first := true
	err = a.Allowance(q, duration, func(o accountpolicyledger.Observation) error {
		if e := c.Request.Context().Err(); e != nil {
			return e
		}
		if !first {
			_, _ = io.WriteString(c.Writer, ",")
		}
		first = false
		return json.NewEncoder(c.Writer).Encode(accountpolicyledger.PublicObservation(o))
	})
	if err != nil {
		return
	}
	_, _ = io.WriteString(c.Writer, ` ],"status":`)
	_ = json.NewEncoder(c.Writer).Encode(a.Status())
	_, _ = io.WriteString(c.Writer, `}`)
}
