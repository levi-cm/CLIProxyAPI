// Package codexcapacity recognizes admission pressure independently of allowance.
package codexcapacity

import (
	"github.com/tidwall/gjson"
	"strings"
)

func IsCapacity(body []byte) bool {
	// Genuine allowance exhaustion is authoritative even if its message mentions capacity.
	for _, path := range []string{"error.code", "error.type", "code", "type"} {
		if gjson.GetBytes(body, path).String() == "usage_limit_reached" {
			return false
		}
	}
	for _, path := range []string{"error.code", "code"} {
		if gjson.GetBytes(body, path).String() == "turn_rate_limit_exceeded" {
			return true
		}
	}
	for _, path := range []string{"error.message", "message"} {
		lower := strings.ToLower(gjson.GetBytes(body, path).String())
		if strings.Contains(lower, "codex is at capacity") || strings.Contains(lower, "model_at_capacity") || strings.Contains(lower, "model_is_at_capacity") || strings.Contains(lower, "model") && strings.Contains(lower, "at capacity") {
			return true
		}
	}
	return false
}
