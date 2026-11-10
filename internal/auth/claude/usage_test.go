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

// The limit list mirrors the live usage response: the session (5-hour) limit,
// the weekly limit and the Fable-scoped weekly limit.
const usageWithLimits = `{"five_hour":null,"seven_day":{"utilization":61.0,"resets_at":"2026-11-11T14:00:00.5+00:00"},
"limits":[
 {"kind":"session","group":"session","percent":100,"severity":"critical","resets_at":"2026-11-10T20:10:00.5+00:00","scope":null,"is_active":true},
 {"kind":"weekly_all","group":"weekly","percent":61,"resets_at":"2026-11-11T14:00:00.5+00:00","scope":null},
 {"kind":"weekly_scoped","group":"weekly","percent":67,"resets_at":"2026-11-11T14:00:00.5+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}
]}`

func TestUsageRateLimitHeadersReadsSessionAndFableLimits(t *testing.T) {
	var usage Usage
	if errDecode := json.Unmarshal([]byte(usageWithLimits), &usage); errDecode != nil {
		t.Fatal(errDecode)
	}
	h := usage.RateLimitHeaders()
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    "1",
		"Anthropic-Ratelimit-Unified-5h-Reset":          "1794341400",
		"Anthropic-Ratelimit-Unified-7d-Utilization":    "0.61",
		"Anthropic-Ratelimit-Unified-7d-Reset":          "1794405600",
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.67",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       "1794405600",
	}
	for name, value := range want {
		if got := h.Get(name); got != value {
			t.Fatalf("%s = %q want %q", name, got, value)
		}
	}
	if len(h) != len(want) {
		t.Fatalf("unexpected headers: %v", h)
	}
}

func TestUsageRateLimitHeadersPrefersFiveHourField(t *testing.T) {
	var usage Usage
	body := `{"five_hour":{"utilization":40,"resets_at":"2026-11-10T20:10:00Z"},"limits":[{"kind":"session","percent":100,"resets_at":"2026-11-10T21:10:00Z","scope":null},{"kind":"weekly_scoped","percent":90,"resets_at":"2026-11-13T15:00:00Z","scope":{"model":{"display_name":"Opus"}}}]}`
	if errDecode := json.Unmarshal([]byte(body), &usage); errDecode != nil {
		t.Fatal(errDecode)
	}
	h := usage.RateLimitHeaders()
	if got := h.Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "0.4" {
		t.Fatalf("5h utilization = %q", got)
	}
	if got := h.Get("Anthropic-Ratelimit-Unified-7d_oi-Utilization"); got != "" {
		t.Fatalf("a non-Fable scoped limit became the Fable window: %q", got)
	}
}
