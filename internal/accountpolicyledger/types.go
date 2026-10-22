// Package accountpolicyledger retains allowlisted measurements indefinitely.
// Raw, synced JSONL records are authoritative; indexes can always be rebuilt.
package accountpolicyledger

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"time"
)

const SchemaVersion = 1

type Request struct {
	MeasurementQuality   string                    `json:"measurement_quality"`
	SchemaVersion        int                       `json:"schema_version"`
	RequestID            string                    `json:"request_id"`
	ParentExecutionID    string                    `json:"parent_execution_id,omitempty"`
	TraceID              string                    `json:"trace_id,omitempty"`
	Provider             string                    `json:"provider"`
	CredentialID         string                    `json:"credential_id"`
	RequestedModel       string                    `json:"requested_model"`
	ExecutionModel       string                    `json:"execution_model"`
	ResponseModel        string                    `json:"response_model"`
	RequestedAt          time.Time                 `json:"requested_at"`
	DurationMS           float64                   `json:"duration_ms"`
	Stream               bool                      `json:"stream"`
	Outcome              string                    `json:"outcome"`
	StatusCode           int                       `json:"status_code"`
	Generate             bool                      `json:"generate"`
	AuthCategory         string                    `json:"auth_category"`
	RequestedServiceTier string                    `json:"requested_service_tier"`
	ResponseServiceTier  string                    `json:"response_service_tier"`
	Tokens               usage.TokenBreakdown      `json:"tokens"`
	Measurements         usage.MeasurementPresence `json:"measurements"`
	CacheWrite5m         *int64                    `json:"cache_write_5m_tokens"`
	CacheWrite1h         *int64                    `json:"cache_write_1h_tokens"`
	CachedInputPercent   *float64                  `json:"cached_input_percent"`
	Legacy               bool                      `json:"legacy,omitempty"`
}

type Observation struct {
	SchemaVersion    int       `json:"schema_version"`
	ID               string    `json:"id"`
	CredentialID     string    `json:"credential_id"`
	IdentityRef      string    `json:"identity_ref"`
	Provider         string    `json:"provider"`
	Scope            string    `json:"scope"`
	Model            string    `json:"model"`
	DurationSeconds  int64     `json:"duration_seconds"`
	ObservedAt       time.Time `json:"observed_at"`
	UsedPercent      float64   `json:"used_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	Allowed          *bool     `json:"allowed"`
	ResetAt          time.Time `json:"reset_at"`
	Event            string    `json:"event"`
	OperationID      string    `json:"operation_id,omitempty"`
}

type Query struct {
	From         time.Time
	To           time.Time
	Provider     string
	Model        string
	CredentialID string
	Cursor       string
	Limit        int
}

type Page struct {
	SchemaVersion int       `json:"schema_version"`
	Records       []Request `json:"records"`
	NextCursor    string    `json:"next_cursor,omitempty"`
}

type Totals struct {
	Period             string   `json:"period"`
	Provider           string   `json:"provider"`
	Model              string   `json:"model"`
	CredentialID       string   `json:"credential_id"`
	Attempts           uint64   `json:"provider_attempts"`
	Failed             uint64   `json:"failed_attempts"`
	InputTokens        int64    `json:"input_tokens"`
	OutputTokens       int64    `json:"output_tokens"`
	CacheReadTokens    int64    `json:"cache_read_tokens"`
	CacheWriteTokens   int64    `json:"cache_write_tokens"`
	ReasoningTokens    int64    `json:"reasoning_tokens"`
	TotalTokens        int64    `json:"total_tokens"`
	MeasuredAttempts   uint64   `json:"cache_measured_attempts"`
	MeasuredInput      int64    `json:"cache_measured_input_tokens"`
	MeasuredCache      int64    `json:"cache_measured_read_tokens"`
	CachedInputPercent *float64 `json:"cached_input_percent"`
}

type CoverageGap struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

type Status struct {
	ProcessStartedAt  time.Time     `json:"process_started_at"`
	SchemaVersion     int           `json:"schema_version"`
	Enabled           bool          `json:"enabled"`
	Healthy           bool          `json:"healthy"`
	Written           uint64        `json:"written_this_process"`
	WriteErrors       uint64        `json:"write_errors"`
	Duplicates        uint64        `json:"duplicates_this_process"`
	LastWriteAt       time.Time     `json:"last_write_at"`
	CoverageStart     time.Time     `json:"coverage_start"`
	UnhealthySegments []string      `json:"unhealthy_segments"`
	UncertainCoverage []CoverageGap `json:"uncertain_coverage"`
	CoverageNote      string        `json:"coverage_note"`
	Closed            bool          `json:"closed"`
}
