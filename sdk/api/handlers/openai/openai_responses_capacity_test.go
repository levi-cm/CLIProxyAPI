package openai

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/tidwall/gjson"
)

type capacityWireError struct{}

func (capacityWireError) Error() string {
	return `{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later","type":"rate_limit_error"}}`
}
func (capacityWireError) StatusCode() int            { return 429 }
func (capacityWireError) IsCapacity() bool           { return true }
func (capacityWireError) RetryAfter() *time.Duration { d := 120 * time.Second; return &d }
func TestPinnedCapacityErrorReachesWebsocketWire(t *testing.T) {
	msg := &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: capacityWireError{}}
	if shouldReplayResponsesWebsocketPinnedAuthFailure(msg) {
		t.Fatal("capacity suppressed as credential replay")
	}
	if !shouldExposeResponsesUpstreamError(msg) {
		t.Fatal("capacity hidden from downstream")
	}
	payload, err := buildResponsesWebsocketErrorPayload(msg)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(payload, "status").Int() != 429 || gjson.GetBytes(payload, "error.code").String() != "turn_rate_limit_exceeded" || gjson.GetBytes(payload, "headers.Retry-After").String() != "120" {
		t.Fatalf("capacity wire fields lost: %s", payload)
	}
}
