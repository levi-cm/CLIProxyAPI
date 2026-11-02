package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestMergedCompletedStreamRetainsCapturedRequestToken(t *testing.T) {
	hook := &captureResultHook{done: make(chan Result, 1)}
	manager := NewManager(nil, nil, hook)
	ctx := WithSkipPersist(context.Background())
	credential, errRegister := manager.Register(ctx, &Auth{
		ID: "merged-completion-snapshot", Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{
			"access_token": "outer-token", "use_siwc": true,
			"siwc": map[string]any{"access_token": "siwc-token"},
		},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	headers := http.Header{"User-Agent": []string{"custom-client/1.0"}}
	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	stream := manager.wrapStreamResult(ctx, credential, "codex", "fixture-model", "fixture-model", nil, nil, remaining, OAuthModelAliasResult{}, false, cliproxyexecutor.Options{Headers: headers}, 1)
	// Request headers may be reused after dispatch. Completion must use the
	// credential partition captured before asynchronous processing began.
	headers.Set("User-Agent", "codex_cli_rs/1.0")
	remaining <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")}
	close(remaining)
	for range stream.Chunks {
	}
	result := <-hook.done
	if !result.Success || !result.CompletedResponse || result.AccessToken != "siwc-token" {
		t.Fatalf("completion lost request identity: success=%v completed=%v token=%q", result.Success, result.CompletedResponse, result.AccessToken)
	}
	current, _ := manager.GetByID(credential.ID)
	if !current.InferenceVerified {
		t.Fatal("completed inference was not verified")
	}
}
