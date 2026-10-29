package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type committedOwnerStreamError struct{}

func (committedOwnerStreamError) Error() string {
	return "upstream ended after submitting the generation"
}
func (committedOwnerStreamError) StatusCode() int       { return 408 }
func (committedOwnerStreamError) IsRequestScoped() bool { return true }

type committedOwnerStreamExecutor struct{ failOnceStreamExecutor }

func (e *committedOwnerStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created"}` + "\n\n")}
	chunks <- coreexecutor.StreamChunk{Err: committedOwnerStreamError{}}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestHandlerDoesNotReplayCommittedOwnerStream(t *testing.T) {
	e := &committedOwnerStreamExecutor{}
	m := coreauth.NewManager(nil, nil, nil)
	m.RegisterExecutor(e)
	a := &coreauth.Auth{ID: "owner-stream-handler", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "owner-stream-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 3}}, m)
	// The handler may suppress handshake-only output, but that does not prove the
	// upstream turn had no tool effects. Its own retries must not replay it.
	h.SetPluginHost(&handlerInterceptorTestHost{interceptStreamChunk: func(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
		return pluginapi.StreamChunkInterceptResponse{DropChunk: true}
	}})
	data, _, errs := h.ExecuteStreamWithAuthManager(context.Background(), "openai", "owner-stream-model", []byte(`{"input":"hello"}`), "")
	for range data {
	}
	status := 0
	for err := range errs {
		if err != nil {
			status = err.StatusCode
		}
	}
	if e.calls != 1 || status != http.StatusRequestTimeout {
		t.Fatalf("calls=%d status=%d", e.calls, status)
	}
}
