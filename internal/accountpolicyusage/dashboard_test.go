package accountpolicyusage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func observedToken(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}

// A total-only upstream usage record cannot prove zero-valued token components.
func TestDashboardTotalOnlyTokensLeaveComponentsUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sink := openSink(t, t.TempDir(), func() bool { return true })
	sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(-time.Minute), Detail: usage.Detail{TotalTokens: 10}})
	result := sink.Dashboard(now, time.Hour)
	if observedToken(result.Totals.TotalTokens) != 10 || result.Totals.InputTokens != nil || result.Totals.OutputTokens != nil || result.Totals.ReasoningTokens != nil || result.Totals.TokenMeasuredRequests != 1 || result.Totals.TokenMissingRequests != 0 {
		t.Fatalf("total-only record fabricated component measurements: %+v", result.Totals)
	}
	if len(result.Series) != 1 || observedToken(result.Series[0].TotalTokens) != 10 || result.Series[0].InputTokens != nil || result.Series[0].OutputTokens != nil {
		t.Fatalf("bucket fabricated components: %+v", result.Series)
	}
	sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "b", RequestedAt: now.Add(-time.Minute), Detail: usage.Detail{TotalTokens: 20, InputTokens: 5, OutputTokens: 15, ReasoningTokens: 3}})
	result = sink.Dashboard(now, time.Hour)
	if observedToken(result.Totals.TotalTokens) != 30 || result.Totals.InputTokens != nil || result.Totals.OutputTokens != nil || result.Totals.ReasoningTokens != nil {
		t.Fatalf("partial components claimed complete sums: %+v", result.Totals)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	restart := openSink(t, sink.dir, func() bool { return false })
	result = restart.Dashboard(now, time.Hour)
	if observedToken(result.Totals.TotalTokens) != 30 || result.Totals.InputTokens != nil || result.Totals.OutputTokens != nil || result.Totals.ReasoningTokens != nil {
		t.Fatalf("restart changed component uncertainty: %+v", result.Totals)
	}
}

// Each component has independent evidence: an unknown reasoning value must not hide input/output.
func TestDashboardTokenComponentsRequireEveryIncludedRecord(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sink := openSink(t, t.TempDir(), func() bool { return true })
	for i, detail := range []usage.Detail{{TotalTokens: 15, InputTokens: 10, OutputTokens: 5, ReasoningTokens: 2}, {TotalTokens: 30, InputTokens: 20, OutputTokens: 10}} {
		sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: now.Add(-time.Minute), Detail: detail})
		if i == 0 {
			single := sink.Dashboard(now, time.Hour)
			if observedToken(single.Totals.TotalTokens) != 15 || observedToken(single.Totals.InputTokens) != 10 || observedToken(single.Totals.OutputTokens) != 5 || observedToken(single.Totals.ReasoningTokens) != 2 {
				t.Fatalf("positive component observations unavailable: %+v", single.Totals)
			}
		}
	}
	result := sink.Dashboard(now, time.Hour)
	if observedToken(result.Totals.TotalTokens) != 45 || observedToken(result.Totals.InputTokens) != 30 || observedToken(result.Totals.OutputTokens) != 15 || result.Totals.ReasoningTokens != nil {
		t.Fatalf("per-field coverage was guessed: %+v", result.Totals)
	}
}

// Missing upstream token measurements must not become a measured zero total.
func TestDashboardMissingTokenMeasurementsAreNullable(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sink := openSink(t, t.TempDir(), func() bool { return true })
	sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(-time.Minute), Latency: time.Millisecond})
	decode := func() map[string]any {
		data, err := json.Marshal(sink.Dashboard(now, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := decode()
	totals := result["totals"].(map[string]any)
	if totals["requests"] != float64(1) || totals["token_measured_requests"] != float64(0) || totals["token_missing_requests"] != float64(1) || totals["total_tokens"] != nil || totals["input_tokens"] != nil || totals["output_tokens"] != nil || totals["reasoning_tokens"] != nil {
		t.Fatalf("missing measurements became zero: %+v", totals)
	}
	series := result["series"].([]any)
	if len(series) != 1 || series[0].(map[string]any)["requests"] != float64(1) || series[0].(map[string]any)["total_tokens"] != nil {
		t.Fatalf("token uncertainty removed request graph: %+v", series)
	}
	sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "b", RequestedAt: now.Add(-time.Minute), Detail: usage.Detail{TotalTokens: 10}})
	result = decode()
	totals = result["totals"].(map[string]any)
	if totals["requests"] != float64(2) || totals["token_measured_requests"] != float64(1) || totals["token_missing_requests"] != float64(1) || totals["total_tokens"] != nil {
		t.Fatalf("partial token evidence claimed a complete sum: %+v", totals)
	}
	accounts := result["accounts"].([]any)
	if accounts[0].(map[string]any)["total_tokens"] != nil || accounts[1].(map[string]any)["total_tokens"] != float64(10) {
		t.Fatalf("account token evidence was mixed: %+v", accounts)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := openSink(t, sink.dir, func() bool { return false })
	data, err := json.Marshal(restarted.Dashboard(now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	totals = result["totals"].(map[string]any)
	if totals["total_tokens"] != nil || totals["token_missing_requests"] != float64(1) {
		t.Fatalf("restart fabricated zero measurements: %+v", totals)
	}
}

// A missing retained completion, incorrect window, or duplicate inflates these hand-derived totals.
func TestDashboardRetainedCompletionsAndWindows(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	sink := openSink(t, t.TempDir(), func() bool { return true })
	first := usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(-30 * time.Minute), Latency: 100 * time.Millisecond, Detail: usage.Detail{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 2, TotalTokens: 15}}
	sink.HandleUsage(context.Background(), first)
	sink.HandleUsage(context.Background(), first)
	sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "b", RequestedAt: now.Add(-5 * time.Minute), Latency: 300 * time.Millisecond, Failed: true, Detail: usage.Detail{InputTokens: 20, OutputTokens: 10, TotalTokens: 30}})
	sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(-2 * time.Hour), Latency: time.Second, Detail: usage.Detail{TotalTokens: 9}})
	s := sink.Dashboard(now, time.Hour)
	if !s.Available || !s.Collecting || s.Totals.Requests != 2 || s.Totals.Success != 1 || s.Totals.Failed != 1 || observedToken(s.Totals.TotalTokens) != 45 || observedToken(s.Totals.InputTokens) != 30 || observedToken(s.Totals.OutputTokens) != 15 || s.Totals.ReasoningTokens != nil || s.Totals.AverageLatencyMS == nil || *s.Totals.AverageLatencyMS != 200 {
		t.Fatalf("wrong retained totals: %+v", s)
	}
	if s.CoverageStart == nil || !s.CoverageStart.Equal(now.Add(-2*time.Hour)) || s.CoverageEnd == nil || !s.CoverageEnd.Equal(now.Add(-5*time.Minute)) || s.RangeSeconds != 3600 || s.BucketSeconds != 60 {
		t.Fatalf("wrong coverage: %+v", s)
	}
	if len(s.Series) != 2 || s.Series[0].Requests != 1 || observedToken(s.Series[0].TotalTokens) != 15 || s.Series[1].Failed != 1 || len(s.Accounts) != 2 || s.Accounts[0].CredentialID != "a" || observedToken(s.Accounts[1].TotalTokens) != 30 {
		t.Fatalf("wrong buckets/accounts: %+v", s)
	}
	day := sink.Dashboard(now, 24*time.Hour)
	if day.Totals.Requests != 3 || observedToken(day.Totals.TotalTokens) != 54 || day.BucketSeconds != 900 {
		t.Fatalf("wrong day: %+v", day)
	}
	// Returned pointers/slices must not let a reader corrupt the cached aggregate.
	*s.Totals.AverageLatencyMS = 999
	s.Accounts[0].Requests = 999
	if again := sink.Dashboard(now, time.Hour); *again.Totals.AverageLatencyMS != 200 || again.Accounts[0].Requests != 1 {
		t.Fatal("reader mutated retained summary")
	}
}

// Concurrent readers must neither consume observations nor race incremental writer aggregation.
func TestDashboardConcurrentReadersPreserveCompletions(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sink := openSink(t, t.TempDir(), func() bool { return true })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(-time.Minute), Detail: usage.Detail{TotalTokens: 2}})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				snapshot := sink.Dashboard(now, time.Hour)
				if !snapshot.Available || snapshot.Totals.Requests > 64 || observedToken(snapshot.Totals.TotalTokens) != int64(snapshot.Totals.Requests)*2 {
					t.Errorf("inconsistent concurrent snapshot: %+v", snapshot)
				}
			}
		}()
	}
	wg.Wait()
	if s := sink.Dashboard(now, time.Hour); s.Totals.Requests != 64 || observedToken(s.Totals.TotalTokens) != 128 {
		t.Fatalf("lost concurrent completions: %+v", s)
	}
}

// Dashboard reads must never enable collection, create directories, or truncate crash tails.
func TestDashboardDisabledReadOnlyAndUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "absent")
	sink := openSink(t, dir, func() bool { return false })
	s := sink.Dashboard(now, time.Hour)
	if s.Available || s.Collecting || s.CoverageStart != nil || s.CoverageEnd != nil || s.Totals.AverageLatencyMS != nil || s.Series == nil || s.Accounts == nil {
		t.Fatalf("fabricated observations: %+v", s)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read created directory: %v", err)
	}
	dir = t.TempDir()
	sink = openSink(t, dir, func() bool { return true })
	if s = sink.Dashboard(now, time.Hour); !s.Available || !s.Collecting || s.Totals.Requests != 0 {
		t.Fatalf("collecting empty sink: %+v", s)
	}
}

// Rotation and restart must expose only the still-retained events, even when collection is off.
func TestDashboardRotationRestartAndReadCache(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	sink.maxFileBytes = 600
	for i := 0; i < 5; i++ {
		sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(time.Duration(i-10) * time.Minute), Latency: time.Millisecond, Detail: usage.Detail{TotalTokens: int64(i + 1)}})
		_ = sink.Dashboard(now, time.Hour)
	}
	s := sink.Dashboard(now, time.Hour)
	if s.Totals.Requests != 3 || observedToken(s.Totals.TotalTokens) != 12 || !s.CoverageStart.Equal(now.Add(-8*time.Minute)) {
		t.Fatalf("rotation kept discarded events: %+v", s)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	restart := openSink(t, dir, func() bool { return false })
	s = restart.Dashboard(now, time.Hour)
	if !s.Available || s.Collecting || s.Totals.Requests != 3 || observedToken(s.Totals.TotalTokens) != 12 {
		t.Fatalf("lost retained restart history: %+v", s)
	}
	// The read cache is owned by this sink; live appends/rotation update it incrementally.
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte("incomplete crash tail"), 0600); err != nil {
		t.Fatal(err)
	}
	if again := restart.Dashboard(now, time.Hour); observedToken(again.Totals.TotalTokens) != 12 {
		t.Fatal("snapshot rescanned unchanged sink files")
	}
	bytes, err := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if err != nil || string(bytes) != "incomplete crash tail" {
		t.Fatal("dashboard modified retained files")
	}
}

// Exact window edges must not round an older event into the requested range.
func TestDashboardExactEdgesAndMalformedRetention(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	sink := openSink(t, t.TempDir(), func() bool { return true })
	for _, at := range []time.Time{now.Add(-time.Hour - time.Second), now.Add(-time.Hour), now.Add(time.Second)} {
		sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: at})
	}
	if s := sink.Dashboard(now, time.Hour); s.Totals.Requests != 1 {
		t.Fatalf("incorrect inclusive edges: %+v", s)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte("bad complete event\n"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := openSink(t, dir, func() bool { return false })
	if s := broken.Dashboard(now, time.Hour); s.Available || s.Totals.Requests != 0 {
		t.Fatalf("corruption reported as evidence: %+v", s)
	}
}

// Read-only restart inspection must ignore, rather than repair, an unfinished final event.
func TestDashboardCrashTailAndSymlinkRemainUntouched(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writer := openSink(t, dir, func() bool { return true })
	writer.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), AuthID: "a", RequestedAt: now.Add(-time.Minute), Detail: usage.Detail{TotalTokens: 5}})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "usage.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(`{"incomplete":`)...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reader := openSink(t, dir, func() bool { return false })
	if s := reader.Dashboard(now, time.Hour); !s.Available || s.Collecting || s.Totals.Requests != 1 || observedToken(s.Totals.TotalTokens) != 5 {
		t.Fatalf("tail corrupted complete evidence: %+v", s)
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(data) {
		t.Fatal("dashboard repaired crash tail")
	}
	unsafeDir := t.TempDir()
	if err := os.Symlink(path, filepath.Join(unsafeDir, "usage.jsonl")); err != nil {
		t.Fatal(err)
	}
	unsafe := openSink(t, unsafeDir, func() bool { return true })
	if s := unsafe.Dashboard(now, time.Hour); s.Available || s.Totals.Requests != 0 || s.CoverageStart != nil {
		t.Fatalf("symlink exposed outside observations: %+v", s)
	}
	retained, err = os.ReadFile(path)
	if err != nil || string(retained) != string(data) {
		t.Fatal("dashboard changed symlink target")
	}
}
