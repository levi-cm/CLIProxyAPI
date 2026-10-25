package auth

import (
	"context"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestCodexClientClassificationSurvivesGinContextReuse(t *testing.T) {
	for _, original := range []string{"codex-desktop/1.0", "ordinary-client"} {
		t.Run(original, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/responses", nil)
			request.Header.Set("User-Agent", original)
			pooled := &gin.Context{Request: request}
			ctx := withCodexClientRequestSnapshot(context.WithValue(context.Background(), "gin", pooled), nil)
			replacement := httptest.NewRequest("POST", "/v1/responses", nil)
			if original == "ordinary-client" {
				replacement.Header.Set("User-Agent", "codex-desktop/2.0")
			}
			pooled.Request = replacement
			if got, want := IsCodexClientRequest(ctx), IsCodexClientUserAgent(original); got != want {
				t.Fatalf("classification=%v want %v after pool reuse", got, want)
			}
		})
	}
}
