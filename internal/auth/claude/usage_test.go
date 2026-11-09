package claude

import (
	"encoding/json"
	"testing"
)

func TestUsageRateLimitHeaders(t *testing.T) {
	var usage Usage
	body := `{"five_hour":null,"seven_day":{"utilization":55.0,"resets_at":"2026-11-13T15:00:00.441847+00:00"},"seven_day_opus":null}`
	if errDecode := json.Unmarshal([]byte(body), &usage); errDecode != nil {
		t.Fatal(errDecode)
	}
	h := usage.RateLimitHeaders()
	if got := h.Get("Anthropic-Ratelimit-Unified-7d-Utilization"); got != "0.55" {
		t.Fatalf("7d utilization = %q", got)
	}
	if got := h.Get("Anthropic-Ratelimit-Unified-7d-Reset"); got != "1794582000" {
		t.Fatalf("7d reset = %q", got)
	}
	if len(h) != 2 {
		t.Fatalf("absent five-hour window must not be emitted: %v", h)
	}
}

func TestUsageRateLimitHeadersSkipsInvalidWindows(t *testing.T) {
	var usage Usage
	body := `{"five_hour":{"utilization":37,"resets_at":"bad"},"seven_day":{"utilization":null,"resets_at":"2026-11-13T15:00:00Z"}}`
	if errDecode := json.Unmarshal([]byte(body), &usage); errDecode != nil {
		t.Fatal(errDecode)
	}
	if h := usage.RateLimitHeaders(); len(h) != 0 {
		t.Fatalf("invalid windows emitted headers: %v", h)
	}
	var nilUsage *Usage
	if len(nilUsage.RateLimitHeaders()) != 0 {
		t.Fatal("nil usage emitted headers")
	}
}
