package accountpolicyusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Metrics describe retained usage records, never provider allowance. Retries or
// additional models may emit multiple records for one downstream request.
type Metrics struct {
	Requests              uint64   `json:"requests"`
	Success               uint64   `json:"success"`
	Failed                uint64   `json:"failed"`
	TotalTokens           *int64   `json:"total_tokens"`
	InputTokens           *int64   `json:"input_tokens"`
	OutputTokens          *int64   `json:"output_tokens"`
	TokenMeasuredRequests uint64   `json:"token_measured_requests"`
	TokenMissingRequests  uint64   `json:"token_missing_requests"`
	AverageLatencyMS      *float64 `json:"average_latency_ms"`
}

type Totals struct {
	Metrics
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

type SeriesPoint struct {
	At time.Time `json:"at"`
	Metrics
}

type AccountSummary struct {
	CredentialID          string   `json:"credential_id"`
	Requests              uint64   `json:"requests"`
	Success               uint64   `json:"success"`
	Failed                uint64   `json:"failed"`
	TotalTokens           *int64   `json:"total_tokens"`
	TokenMeasuredRequests uint64   `json:"token_measured_requests"`
	TokenMissingRequests  uint64   `json:"token_missing_requests"`
	AverageLatencyMS      *float64 `json:"average_latency_ms"`
}

// DashboardSnapshot explicitly distinguishes missing retained evidence from zero.
// Coverage spans the earliest/latest retained usage-record request timestamps,
// not a promise that every usage record within that interval was retained.
type DashboardSnapshot struct {
	Available     bool             `json:"available"`
	Collecting    bool             `json:"collecting"`
	CoverageStart *time.Time       `json:"coverage_start"`
	CoverageEnd   *time.Time       `json:"coverage_end"`
	RangeSeconds  int64            `json:"range_seconds"`
	BucketSeconds int64            `json:"bucket_seconds"`
	Totals        Totals           `json:"totals"`
	Series        []SeriesPoint    `json:"series"`
	Accounts      []AccountSummary `json:"accounts"`
}

type dashboardAggregate struct {
	requests, failed, measured                       uint64
	inputMeasured, outputMeasured, reasoningMeasured uint64
	total, input, output, reasoning                  int64
	latency                                          float64
}

func (a *dashboardAggregate) merge(b dashboardAggregate) {
	a.requests += b.requests
	a.failed += b.failed
	a.measured += b.measured
	a.inputMeasured += b.inputMeasured
	a.outputMeasured += b.outputMeasured
	a.reasoningMeasured += b.reasoningMeasured
	a.total += b.total
	a.input += b.input
	a.output += b.output
	a.reasoning += b.reasoning
	a.latency += b.latency
}

func (a dashboardAggregate) metrics() Metrics {
	m := Metrics{Requests: a.requests, Success: a.requests - a.failed, Failed: a.failed, TokenMeasuredRequests: a.measured, TokenMissingRequests: a.requests - a.measured}
	if a.requests == a.measured {
		total := a.total
		m.TotalTokens = &total
	}
	// Schema 1 cannot distinguish an unreported component from measured zero.
	// Only positive observations for every included record establish its sum.
	if a.input > 0 && a.inputMeasured == a.requests {
		input := a.input
		m.InputTokens = &input
	}
	if a.output > 0 && a.outputMeasured == a.requests {
		output := a.output
		m.OutputTokens = &output
	}
	if a.requests > 0 {
		average := a.latency / float64(a.requests)
		m.AverageLatencyMS = &average
	}
	return m
}

type dashboardSample struct {
	at           time.Time
	credentialID string
	value        dashboardAggregate
}

type dashboardBucket struct {
	value       dashboardAggregate
	accounts    map[string]dashboardAggregate
	samples     []dashboardSample
	first, last time.Time
}

func (s *Sink) addDashboardEvent(segment int, item event) {
	if item.RequestedAt.IsZero() {
		return
	}
	if s.dashboardSegments[segment] == nil {
		s.dashboardSegments[segment] = make(map[int64]*dashboardBucket)
	}
	key := item.RequestedAt.Unix() / 60
	bucket := s.dashboardSegments[segment][key]
	if bucket == nil {
		bucket = &dashboardBucket{accounts: make(map[string]dashboardAggregate), first: item.RequestedAt, last: item.RequestedAt}
		s.dashboardSegments[segment][key] = bucket
	}
	value := dashboardAggregate{requests: 1, total: item.TotalTokens, input: item.InputTokens, output: item.OutputTokens, reasoning: item.ReasoningTokens, latency: item.LatencyMS}
	// Legacy zero totals cannot establish measured zero: EnsurePublished and
	// missing-usage failures produce the same values. Never infer token evidence.
	if item.TotalTokens > 0 {
		value.measured = 1
	}
	if item.InputTokens > 0 {
		value.inputMeasured = 1
	}
	if item.OutputTokens > 0 {
		value.outputMeasured = 1
	}
	if item.ReasoningTokens > 0 {
		value.reasoningMeasured = 1
	}
	if item.Failed {
		value.failed = 1
	}
	bucket.value.merge(value)
	account := bucket.accounts[item.CredentialID]
	account.merge(value)
	bucket.accounts[item.CredentialID] = account
	bucket.samples = append(bucket.samples, dashboardSample{at: item.RequestedAt, credentialID: item.CredentialID, value: value})
	if item.RequestedAt.Before(bucket.first) {
		bucket.first = item.RequestedAt
	}
	if item.RequestedAt.After(bucket.last) {
		bucket.last = item.RequestedAt
	}
}

// loadDashboard reads bounded segments once without recovery, chmod, mkdir, or
// queue draining. Subsequent appends and rotations update aggregates in memory.
func (s *Sink) loadDashboard() error {
	if err := checkDirectory(s.dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if errClose := root.Close(); errClose != nil {
			log.WithField("component", "account-policy-usage").Error("usage read directory close failed")
		}
	}()
	for i, name := range segmentNames {
		info, errStat := root.Lstat(name)
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil {
			return errStat
		}
		if !info.Mode().IsRegular() || info.Size() > s.maxFileBytes {
			return errors.New("usage segment must be bounded and regular")
		}
		file, errOpen := root.Open(name)
		if errOpen != nil {
			return errOpen
		}
		data, errRead := io.ReadAll(io.LimitReader(file, s.maxFileBytes+1))
		errClose := file.Close()
		if err := errors.Join(errRead, errClose); err != nil {
			return err
		}
		if int64(len(data)) > s.maxFileBytes {
			return errors.New("usage segment exceeds bound")
		}
		// An incomplete crash tail carries no durable complete observation.
		data = data[:bytes.LastIndexByte(data, '\n')+1]
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var item event
			if err := json.Unmarshal(line, &item); err != nil {
				return errors.New("invalid usage event")
			}
			if id, err := uuid.Parse(item.RequestID); err != nil || id == uuid.Nil || item.SchemaVersion != 1 {
				return errors.New("invalid usage schema or request ID")
			}
			s.addDashboardEvent(i, item)
		}
	}
	return nil
}

// Dashboard returns a detached snapshot. Only two boundary minutes require
// per-event filtering; interior minutes reuse incrementally maintained totals.
func (s *Sink) Dashboard(now time.Time, window time.Duration) DashboardSnapshot {
	bucketSeconds := int64(60)
	if window > time.Hour {
		bucketSeconds = 900
	}
	if window > 24*time.Hour {
		bucketSeconds = 7200
	}
	result := DashboardSnapshot{RangeSeconds: int64(window / time.Second), BucketSeconds: bucketSeconds, Series: []SeriesPoint{}, Accounts: []AccountSummary{}}
	if s == nil {
		return result
	}
	collecting := s.enabled != nil && s.enabled()
	s.mu.Lock()
	defer s.mu.Unlock()
	result.Collecting = collecting && !s.closed
	if !s.dashboardLoaded {
		s.dashboardSegments = [3]map[int64]*dashboardBucket{}
		s.dashboardValid = s.loadDashboard() == nil
		s.dashboardLoaded = true
	}
	if !s.dashboardValid {
		return result
	}
	start := now.Add(-window)
	var total dashboardAggregate
	series := make(map[int64]dashboardAggregate)
	accounts := make(map[string]dashboardAggregate)
	add := func(at time.Time, value dashboardAggregate, perAccount map[string]dashboardAggregate) {
		total.merge(value)
		key := at.Unix() / bucketSeconds * bucketSeconds
		point := series[key]
		point.merge(value)
		series[key] = point
		for id, value := range perAccount {
			account := accounts[id]
			account.merge(value)
			accounts[id] = account
		}
	}
	for _, segment := range s.dashboardSegments {
		for key, bucket := range segment {
			if result.CoverageStart == nil || bucket.first.Before(*result.CoverageStart) {
				at := bucket.first
				result.CoverageStart = &at
			}
			if result.CoverageEnd == nil || bucket.last.After(*result.CoverageEnd) {
				at := bucket.last
				result.CoverageEnd = &at
			}
			minute := time.Unix(key*60, 0).UTC()
			if !minute.Before(start) && minute.Add(time.Minute).Before(now) {
				add(minute, bucket.value, bucket.accounts)
				continue
			}
			if minute.Add(time.Minute).Before(start) || minute.After(now) {
				continue
			}
			for _, sample := range bucket.samples {
				if !sample.at.Before(start) && !sample.at.After(now) {
					add(sample.at, sample.value, map[string]dashboardAggregate{sample.credentialID: sample.value})
				}
			}
		}
	}
	result.Available = result.Collecting || result.CoverageStart != nil
	result.Totals = Totals{Metrics: total.metrics()}
	if total.reasoning > 0 && total.requests == total.reasoningMeasured {
		reasoning := total.reasoning
		result.Totals.ReasoningTokens = &reasoning
	}
	for at, value := range series {
		result.Series = append(result.Series, SeriesPoint{At: time.Unix(at, 0).UTC(), Metrics: value.metrics()})
	}
	sort.Slice(result.Series, func(i, j int) bool { return result.Series[i].At.Before(result.Series[j].At) })
	for id, value := range accounts {
		m := value.metrics()
		result.Accounts = append(result.Accounts, AccountSummary{CredentialID: id, Requests: m.Requests, Success: m.Success, Failed: m.Failed, TotalTokens: m.TotalTokens, TokenMeasuredRequests: m.TokenMeasuredRequests, TokenMissingRequests: m.TokenMissingRequests, AverageLatencyMS: m.AverageLatencyMS})
	}
	sort.Slice(result.Accounts, func(i, j int) bool { return result.Accounts[i].CredentialID < result.Accounts[j].CredentialID })
	return result
}
