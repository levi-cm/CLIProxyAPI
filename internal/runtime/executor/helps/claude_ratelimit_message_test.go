package helps

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func TestDescribeClaudeRateLimitError(t *testing.T) {
	const body = `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_fixture","extra":{"keep":true}}`
	for _, tt := range []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"five-hour rejection", http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}}, "Claude 5-hour limit reached. Please try again after the limit resets."},
		{"weekly rejection", http.Header{"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"}}, "Claude weekly limit reached. Please try again after the limit resets."},
		{"both rejected", http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-7d-Status": {"rejected"}}, "Claude 5-hour and weekly limits reached. Please try again after the limits reset."},
		{"aggregate five-hour claim", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"}}, "Claude 5-hour limit reached. Please try again after the limit resets."},
		{"aggregate weekly claim", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day"}}, "Claude weekly limit reached. Please try again after the limit resets."},
		{"header casing and whitespace", http.Header{"anthropic-ratelimit-unified-5h-status": {" REJECTED "}}, "Claude 5-hour limit reached. Please try again after the limit resets."},
		{"no headers", nil, ""},
		{"unidentified aggregate", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}, ""},
		{"warning at 99 percent", http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed_warning"}, "Anthropic-Ratelimit-Unified-5h-Status": {"allowed_warning"}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"0.99"}, "Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"}}, ""},
		{"contradictory allowed window", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"}}, ""},
		{"malformed window status", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Status": {"invalid"}, "Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"}}, ""},
		{"Fable-only rejection", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-7d-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"}}, ""},
		{"overage-only rejection", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-7d-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"0.00"}, "Anthropic-Ratelimit-Unified-Overage-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte(body)
			got := DescribeClaudeRateLimitError(tt.headers, original)
			if string(original) != body {
				t.Fatal("mutated the original upstream body")
			}
			if tt.want == "" {
				if !bytes.Equal(got, original) {
					t.Fatalf("unconfirmed limit changed the error: %s", got)
				}
				return
			}
			if message := gjson.GetBytes(got, "error.message").String(); message != tt.want {
				t.Fatalf("message = %q, want %q", message, tt.want)
			}
			for _, path := range []string{"type", "error.type", "request_id", "extra"} {
				if gjson.GetBytes(got, path).Raw != gjson.Get(body, path).Raw {
					t.Fatalf("changed upstream field %s: %s", path, got)
				}
			}
		})
	}
}

func TestDescribeClaudeRateLimitErrorPreservesSpecificOrMalformedErrors(t *testing.T) {
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}}
	for _, body := range []string{
		`{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode."}}`,
		`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed the requests per minute rate limit. Please try again later."}}`,
		`{"type":"error","error":{"type":"authentication_error","message":"This request would exceed your account's rate limit. Please try again later."}}`,
		`{"error":`,
	} {
		if got := DescribeClaudeRateLimitError(headers, []byte(body)); string(got) != body {
			t.Fatalf("changed specific or malformed error: %s", got)
		}
	}
}
