package helps

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DescribeClaudeRateLimitError replaces Anthropic's generic account-limit message
// only when the same rejected response identifies the exhausted usage window.
func DescribeClaudeRateLimitError(headers http.Header, body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	if root.Get("error.type").String() != "rate_limit_error" ||
		root.Get("error.message").String() != "This request would exceed your account's rate limit. Please try again later." {
		return body
	}
	if !ClaudeHeadersIndicateUnifiedRateLimitRejection(headers) {
		return body
	}
	status := func(name string) string {
		return strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, name)))
	}
	fiveHourStatus := status("Anthropic-Ratelimit-Unified-5h-Status")
	weeklyStatus := status("Anthropic-Ratelimit-Unified-7d-Status")
	fiveHour := fiveHourStatus == "rejected"
	weekly := weeklyStatus == "rejected"
	// A rejected aggregate claim can identify a window when its individual
	// status is absent. Allowed or malformed individual statuses are not proof.
	if status("Anthropic-Ratelimit-Unified-Status") == "rejected" {
		switch status("Anthropic-Ratelimit-Unified-Representative-Claim") {
		case "five_hour":
			fiveHour = fiveHour || fiveHourStatus == ""
		case "seven_day":
			weekly = weekly || weeklyStatus == ""
		}
	}
	var message string
	switch {
	case fiveHour && weekly:
		message = "Claude 5-hour and weekly limits reached. Please try again after the limits reset."
	case fiveHour:
		message = "Claude 5-hour limit reached. Please try again after the limit resets."
	case weekly:
		message = "Claude weekly limit reached. Please try again after the limit resets."
	default:
		return body
	}
	updated, errSet := sjson.SetBytes(body, "error.message", message)
	if errSet != nil {
		return body
	}
	return updated
}
