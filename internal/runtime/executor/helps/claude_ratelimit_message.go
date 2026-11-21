package helps

import (
	"net/http"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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
	// Shared with automatic Claude resets, which attribute weekly refusals the
	// same way.
	fiveHour, weekly := cliproxyauth.ClaudeRejectedWindows(headers)
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
