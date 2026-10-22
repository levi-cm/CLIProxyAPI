package helps

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUsagePresenceZeroAbsentAndPartialStream(t *testing.T) {
	zero := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}`))
	absent := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":0}}`))
	if !zero.Measurements.Input || !zero.Measurements.CacheRead || absent.Measurements.CacheRead {
		t.Fatal("zero/absent cache presence lost")
	}
	a := ParseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}}`))
	b := ParseClaudeUsage([]byte(`{"usage":{"output_tokens":20,"cache_read_input_tokens":0}}`))
	m := MergeStreamUsageDetail(a, b)
	if m.InputTokens != 10 || m.CacheReadTokens != 0 || !m.Measurements.CacheRead || m.CacheWrite5mTokens != 3 || m.CacheWrite1hTokens != 4 {
		t.Fatalf("merge %+v", m)
	}
}

func TestPartialOpenAIUsageKeepsPresenceAndReportedZero(t *testing.T) {
	var b StreamUsageBuffer
	first := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":100}}}`))
	second := ParseOpenAIUsage([]byte(`{"usage":{"output_tokens":20}}`))
	b.Observe(first, true)
	b.Observe(second, true)
	d, _ := b.Detail()
	if d.InputTokens != 1000 || d.CacheReadTokens != 100 || !d.Measurements.CacheRead || !d.TokenBreakdown.Valid() || d.TokenBreakdown.TotalTokens != 1020 {
		t.Fatalf("partial usage %+v", d)
	}
	b.Observe(ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":0}}}`)), true)
	d, _ = b.Detail()
	if d.CacheReadTokens != 0 || !d.Measurements.CacheRead {
		t.Fatalf("reported zero %+v", d)
	}
}

func TestPartialReportedTotalsAndZeroTier(t *testing.T) {
	var b StreamUsageBuffer
	first := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":100,"total_tokens":150}}`))
	b.Observe(first, true)
	b.Observe(ParseOpenAIUsage([]byte(`{"usage":{"output_tokens":20}}`)), true)
	d, _ := b.Detail()
	if d.TotalTokens != 150 || !d.Measurements.Total || d.TokenBreakdown.Quality == "complete" {
		t.Fatalf("reported total lost %+v", d)
	}
	zero := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0}}}`))
	zero.ResponseServiceTier = "default"
	b.Observe(zero, true)
	d, _ = b.Detail()
	if d.InputTokens != 0 || d.OutputTokens != 0 || d.TotalTokens != 0 {
		t.Fatalf("zero frame treated as tier-only %+v", d)
	}
}
func TestReasoningOnlyPartialFrameKeepsInput(t *testing.T) {
	var b StreamUsageBuffer
	b.Observe(ParseGeminiUsage([]byte(`{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20}}`)), true)
	b.Observe(ParseGeminiUsage([]byte(`{"usageMetadata":{"thoughtsTokenCount":5}}`)), true)
	d, _ := b.Detail()
	if d.InputTokens != 100 || d.OutputTokens != 20 || d.ReasoningTokens != 5 {
		t.Fatalf("reasoning frame lost earlier measurements %+v", d)
	}
}

func TestUsageRetainsObservedHTTPStatus(t *testing.T) {
	r := NewUsageReporter(context.Background(), "codex", "m", nil)
	r.ObserveResponse(&http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(""))})
	record := r.buildRecord(usage.Detail{}, false)
	if record.ResponseStatusCode != 201 {
		t.Fatalf("observed response status lost: %d", record.ResponseStatusCode)
	}
}

func TestZeroClaudeAndGeminiStreamMeasurementsRemainReported(t *testing.T) {
	a := ParseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":20}}`))
	z := ParseClaudeUsage([]byte(`{"usage":{"input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}`))
	m := MergeStreamUsageDetail(a, z)
	if m.TotalTokens != 0 || m.CacheReadTokens != 0 || !m.TokenBreakdown.Valid() {
		t.Fatalf("Claude reported zeros %+v", m)
	}
	d, ok := ParseGeminiStreamUsage([]byte(`data: {"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"cachedContentTokenCount":0,"totalTokenCount":0}}`))
	if !ok || !d.Measurements.CacheRead || !d.Measurements.Total {
		t.Fatalf("Gemini zero usage discarded %+v %v", d, ok)
	}
}
