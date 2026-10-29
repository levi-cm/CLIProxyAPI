package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"golang.org/x/net/http2"
)

type codexHTTP2FailureReader struct{ failure error }

func (r codexHTTP2FailureReader) Read([]byte) (int, error) { return 0, r.failure }

func TestCodexHTTP2ReadFailurePreservesRecoveryBoundary(t *testing.T) {
	for _, mode := range []string{"empty-nonstream", "partial-nonstream", "bootstrap", "unbuffered-zero-output", "after-output"} {
		t.Run(mode, func(t *testing.T) {
			failure := http2.StreamError{StreamID: 31, Code: http2.ErrCodeInternal}
			data := ""
			if mode == "partial-nonstream" || mode == "after-output" {
				data = "data: " + codexOutputAddedEvent + "\n\n"
			}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader(data), codexHTTP2FailureReader{failure})), Request: req}, nil
			}))
			cfg := &config.Config{}
			cfg.Codex.StreamBootstrapBuffering = mode == "bootstrap"
			executor := NewCodexExecutor(cfg)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://codex.test", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			var err error
			var sawOutput bool
			if mode == "empty-nonstream" || mode == "partial-nonstream" {
				_, err = executor.Execute(ctx, auth, req, opts)
			} else {
				var stream *cliproxyexecutor.StreamResult
				stream, err = executor.ExecuteStream(ctx, auth, req, opts)
				if err == nil {
					for chunk := range stream.Chunks {
						if len(chunk.Payload) > 0 {
							sawOutput = true
						}
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			}
			if err == nil {
				t.Fatal("HTTP2 reset lost")
			}
			if mode == "empty-nonstream" || mode == "bootstrap" || mode == "unbuffered-zero-output" {
				if !errors.Is(err, failure) {
					t.Fatalf("pre-output transport identity lost: %v", err)
				}
			} else if mode == "partial-nonstream" || mode == "after-output" {
				if mode == "after-output" && !sawOutput {
					t.Fatal("expected output before transport failure")
				}
				var truncated interface{ IsRequestScoped() bool }
				if !errors.As(err, &truncated) || !truncated.IsRequestScoped() {
					t.Fatalf("partial generation can be replayed: %v", err)
				}
			}
		})
	}
}
