package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const genericClaudeAccountLimitBody = `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_fixture"}`

func TestClaudeQuotaWindowMessageSurvivesCooldownAndAPI(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
				w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
				w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(genericClaudeAccountLimitBody))
			}))
			defer server.Close()

			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
			auth := &cliproxyauth.Auth{
				ID:       uuid.NewString(),
				Provider: "claude",
				Attributes: map[string]string{
					"api_key":  "fixture-key",
					"base_url": server.URL,
				},
			}
			models := []string{"claude-3-opus-20240229", "claude-3-5-sonnet-20241022"}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: models[0]}, {ID: models[1]}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			for index, model := range models {
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"fixture"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				var errExecute error
				if stream {
					_, errExecute = manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
				} else {
					_, errExecute = manager.Execute(context.Background(), []string{"claude"}, req, opts)
				}
				if errExecute == nil {
					t.Fatal("expected rate-limit error")
				}
				if index == 0 {
					var retry retryAfterProvider
					if !errors.As(errExecute, &retry) || retry.RetryAfter() == nil || *retry.RetryAfter() < time.Hour || *retry.RetryAfter() > time.Hour+35*time.Second {
						t.Fatalf("lost upstream retry duration: %v", errExecute)
					}
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				handler := claudehandlers.NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
				handler.WriteErrorResponse(ctx, &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errExecute})
				if recorder.Code != http.StatusTooManyRequests {
					t.Fatalf("status = %d, want 429", recorder.Code)
				}
				body := recorder.Body.Bytes()
				if got := gjson.GetBytes(body, "error.type").String(); got != "rate_limit_error" {
					t.Fatalf("error.type = %q", got)
				}
				if got := gjson.GetBytes(body, "error.message").String(); !strings.Contains(got, "Claude 5-hour limit reached.") {
					t.Fatalf("client message lost the quota window: %q", got)
				}
				if index > 0 {
					retry, errParse := strconv.Atoi(recorder.Header().Get("Retry-After"))
					if errParse != nil || retry < 3590 || retry > 3635 {
						t.Fatalf("Retry-After = %q, want approximately one hour", recorder.Header().Get("Retry-After"))
					}
				}
				if attempts.Load() != 1 {
					t.Fatalf("upstream attempts = %d, want 1; sibling-model retry must stay local", attempts.Load())
				}
			}
		})
	}
}

func TestClaudeFastDirectQuotaWindowMessage(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			headers := http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
				"Retry-After":                           {"3600"},
				"Content-Length":                        {"999"},
				"Content-Encoding":                      {"gzip"},
			}
			err := newClaudeFastDirectResponseError(&http.Response{StatusCode: status, Header: headers}, []byte(genericClaudeAccountLimitBody))
			var terminated *cliproxyexecutor.RequestTerminatedError
			if !errors.As(err, &terminated) {
				t.Fatalf("expected direct response error, got %T", err)
			}
			if terminated.HTTPStatus != status || terminated.Header.Get("Retry-After") != "3600" {
				t.Fatal("changed status or retry header")
			}
			if terminated.Header.Get("Content-Length") != "" || terminated.Header.Get("Content-Encoding") != "" {
				t.Fatal("forwarded stale representation headers")
			}
			message := gjson.GetBytes(terminated.Body, "error.message").String()
			if status == http.StatusTooManyRequests && !strings.HasPrefix(message, "Claude 5-hour limit reached.") {
				t.Fatalf("fast response lost quota-window message: %s", terminated.Body)
			}
			if status != http.StatusTooManyRequests && string(terminated.Body) != genericClaudeAccountLimitBody {
				t.Fatal("changed a non-429 response")
			}
			if gjson.GetBytes(terminated.Body, "request_id").String() != "req_fixture" {
				t.Fatal("lost upstream request ID")
			}
		})
	}
}
