package handlers

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type committedCapacityError struct{}

func (committedCapacityError) Error() string           { return "Codex is at capacity, please try again later" }
func (committedCapacityError) StatusCode() int         { return 429 }
func (committedCapacityError) IsCapacity() bool        { return true }
func (committedCapacityError) CapacityRetrySafe() bool { return false }

type committedCapacityExecutor struct{ failOnceStreamExecutor }

func (e *committedCapacityExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created"}` + "\n\n")}
	chunks <- coreexecutor.StreamChunk{Err: committedCapacityError{}}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestHandlerDoesNotMultiplyCapacityBudget(t *testing.T) {
	e := &committedCapacityExecutor{}
	m := coreauth.NewManager(nil, nil, nil)
	m.RegisterExecutor(e)
	a := &coreauth.Auth{ID: "capacity-handler", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "capacity-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 3}}, m)
	// The handler may suppress handshake-only output, but that does not prove the
	// upstream turn had no tool effects. Its own retries must not replay it.
	h.SetPluginHost(&handlerInterceptorTestHost{interceptStreamChunk: func(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
		return pluginapi.StreamChunkInterceptResponse{DropChunk: true}
	}})
	data, _, errs := h.ExecuteStreamWithAuthManager(context.Background(), "openai", "capacity-model", []byte(`{"input":"hello"}`), "")
	for range data {
	}
	status := 0
	for err := range errs {
		if err != nil {
			status = err.StatusCode
		}
	}
	if e.calls != 1 || status != http.StatusTooManyRequests {
		t.Fatalf("calls=%d status=%d", e.calls, status)
	}
}

type capacityHintWithoutHeaders struct{ committedCapacityError }

func (capacityHintWithoutHeaders) RetryAfter() *time.Duration { d := 120 * time.Second; return &d }
func TestCapacityExecutionErrorMessageCarriesTrustedHint(t *testing.T) {
	msg := executionErrorMessage(fmt.Errorf("wrapped: %w", capacityHintWithoutHeaders{}))
	if msg.StatusCode != 429 || msg.Addon.Get("Retry-After") != "120" {
		t.Fatalf("wire metadata lost: %+v", msg)
	}
}
