package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestDiagnosisTurnRateLimitClassification(t *testing.T) {
	body := []byte(`{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later","type":"rate_limit_error"}}`)
	err := newCodexStatusErr(http.StatusTooManyRequests, body)
	t.Logf("capacity_recognized=%v retry_hint=%v credential_scoped=%v status=%d", isCodexModelCapacityError(body), err.RetryAfter(), err.IsCredentialScoped(), err.StatusCode())
	if !isCodexModelCapacityError(body) {
		t.Error("upstream turn_rate_limit_exceeded is not recognized by the capacity classifier")
	}
}

func TestDiagnosisCodexDropsHTTPRetryAfter(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later","type":"rate_limit_error"}}`))
			}))
			defer server.Close()
			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "fixture-key", "base_url": server.URL, "plan_type": "pro"}}
			req := cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hello"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
			var err error
			if stream {
				_, err = executor.ExecuteStream(context.Background(), auth, req, opts)
			} else {
				_, err = executor.Execute(context.Background(), auth, req, opts)
			}
			var retry interface{ RetryAfter() *time.Duration }
			if !errors.As(err, &retry) || retry.RetryAfter() == nil {
				t.Fatalf("upstream Retry-After=120 was discarded: error_type=%T error=%v", err, err)
			}
			if *retry.RetryAfter() != 2*time.Minute {
				t.Errorf("got retry hint %v; want 2 minutes", *retry.RetryAfter())
			}
		})
	}
}

func TestCapacityWebsocketRetryHeader(t *testing.T) {
	payload := []byte(`{"type":"error","status":429,"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later"},"headers":{"Retry-After":"120"}}`)
	err, ok := parseCodexWebsocketError(payload)
	if !ok {
		t.Fatal("frame not parsed")
	}
	var hint interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &hint) || hint.RetryAfter() == nil || *hint.RetryAfter() != 120*time.Second {
		t.Fatalf("hint lost: %v", err)
	}
	var capacity interface{ IsCapacity() bool }
	if !errors.As(err, &capacity) || !capacity.IsCapacity() {
		t.Fatalf("classification lost: %v", err)
	}
}

func TestCapacitySSEBootstrapSafetyAndHeader(t *testing.T) {
	for _, tool := range []bool{false, true} {
		t.Run(fmt.Sprint(tool), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "120")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"output\":[]}}\n\n")
				if tool {
					fmt.Fprint(w, "data: {\"type\":\"response.web_search_call.in_progress\"}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"turn_rate_limit_exceeded\",\"message\":\"Codex is at capacity, please try again later\"}}}\n\n")
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Codex.StreamBootstrapBuffering = true
			executor := NewCodexExecutor(cfg)
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"input":"hello"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
			result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
			if tool {
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
				var safe interface{ CapacityRetrySafe() bool }
				if errors.As(err, &safe) && safe.CapacityRetrySafe() {
					t.Fatal("server tool turn marked replayable")
				}
				return
			}
			var hint interface{ RetryAfter() *time.Duration }
			var safe interface{ CapacityRetrySafe() bool }
			if !errors.As(err, &hint) || hint.RetryAfter() == nil || *hint.RetryAfter() != 120*time.Second {
				t.Fatalf("bootstrap hint lost: %v", err)
			}
			if !errors.As(err, &safe) || !safe.CapacityRetrySafe() {
				t.Fatalf("clean bootstrap is not recoverable: %v", err)
			}
		})
	}
}

func TestCapacityWebsocketHandshakeRetryAfter(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later"}}`)
			}))
			defer server.Close()
			e := NewCodexWebsocketsExecutor(&config.Config{})
			a := &cliproxyauth.Auth{ID: "ws-capacity-fixture", Provider: "codex", Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "capacity-handshake"}}
			defer e.CloseExecutionSession("capacity-handshake")
			req := cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"input":"hello"}`)}
			var err error
			if stream {
				_, err = e.ExecuteStream(context.Background(), a, req, opts)
			} else {
				_, err = e.Execute(context.Background(), a, req, opts)
			}
			var hint interface{ RetryAfter() *time.Duration }
			var safe interface{ CapacityRetrySafe() bool }
			if !errors.As(err, &hint) || hint.RetryAfter() == nil || *hint.RetryAfter() != 2*time.Minute {
				t.Fatalf("handshake hint lost: %v", err)
			}
			if !errors.As(err, &safe) || !safe.CapacityRetrySafe() {
				t.Fatal("handshake rejection marked unsafe")
			}
		})
	}
}

func TestCapacityHTTPStatusPreserved(t *testing.T) {
	body := []byte(`{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later"}}`)
	for _, status := range []int{400, 429, 503} {
		err := newCodexHTTPStatusErr(status, body, http.Header{"Retry-After": {"120"}}, false)
		if err.StatusCode() != status {
			t.Errorf("upstream status %d became %d", status, err.StatusCode())
		}
	}
}

func TestCapacityTerminalStreamAlwaysPreservesHint(t *testing.T) {
	for _, mode := range []string{"unbuffered", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "120")
				fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"turn_rate_limit_exceeded\",\"message\":\"Codex is at capacity, please try again later\"}}}\n\n")
			}))
			defer server.Close()
			cfg := &config.Config{}
			if mode == "timeout" {
				cfg.Codex.StreamBootstrapBuffering = true
				cfg.Codex.StreamBootstrapTimeout = "1s"
				now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
				calls := 0
				cleanup := setCodexBootstrapNowForTest(func() time.Time { calls++; return now.Add(time.Duration(calls) * 2 * time.Second) })
				defer cleanup()
			}
			e := NewCodexExecutor(cfg)
			a := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL}}
			result, err := e.ExecuteStream(context.Background(), a, cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
			if err != nil {
				t.Fatal(err)
			}
			var terminal error
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					terminal = chunk.Err
				}
			}
			var retry interface{ RetryAfter() *time.Duration }
			if !errors.As(terminal, &retry) || retry.RetryAfter() == nil || *retry.RetryAfter() != 120*time.Second {
				t.Fatalf("terminal hint dropped: %v", terminal)
			}
			var safe interface{ CapacityRetrySafe() bool }
			if errors.As(terminal, &safe) && safe.CapacityRetrySafe() {
				t.Fatal("committed stream marked replayable")
			}
		})
	}
}
