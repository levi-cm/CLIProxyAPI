package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type acceptedOwnerStreamError struct{}

func (acceptedOwnerStreamError) Error() string {
	return "accepted stream ended before its terminal frame"
}

func TestEncryptedOwnerTruncationIgnoresReceivedErrorCooldownRules(t *testing.T) {
	for _, action := range []string{RequestScopedActionStopAndCooldown, RequestScopedActionContinueAndCooldown} {
		t.Run(action, func(t *testing.T) {
			executor := &ownerTransportExecutor{failure: acceptedOwnerStreamError{}, always: true, bootstrap: true}
			manager, opts := ownerTransportFixture(t, executor)
			for _, auth := range manager.List() {
				auth.Metadata["request_scoped_errors"] = []internalconfig.RequestScopedErrorRule{{Status: http.StatusBadGateway, Match: []string{"accepted stream"}, Action: action}}
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			auth, _ := manager.GetByID("a")
			if got, matched := matchRequestScopedErrorAction(auth, customStatusError{code: http.StatusBadGateway, msg: "accepted stream refusal"}, nil); !matched || got != action {
				t.Fatalf("received upstream refusal lost configured rule: action=%q matched=%v", got, matched)
			}
			request := cliproxyexecutor.Request{Model: "gpt-5", Payload: opts.OriginalRequest}
			_, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, request, opts)
			if statusCodeFromError(errStream) != http.StatusBadGateway {
				t.Fatalf("accepted stream lost 502: %v", errStream)
			}
			assertNoCooldown(t, manager, "a", "gpt-5")
			if !reflect.DeepEqual(executor.owners, []string{"a"}) {
				t.Fatalf("accepted generation replayed: %v", executor.owners)
			}
		})
	}
}
func (acceptedOwnerStreamError) StatusCode() int         { return http.StatusBadGateway }
func (acceptedOwnerStreamError) IsStreamTruncated() bool { return true }

func TestEncryptedOwnerAcceptedStreamIsNeverReplayedOrCooled(t *testing.T) {
	for _, output := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-output", true: "after-output"}[output], func(t *testing.T) {
			executor := &ownerTransportExecutor{
				failure: acceptedOwnerStreamError{}, always: true,
				bootstrap: !output, afterOutput: output,
			}
			manager, opts := ownerTransportFixture(t, executor)
			request := cliproxyexecutor.Request{Model: "gpt-5", Payload: opts.OriginalRequest}
			for turn := 0; turn < 2; turn++ {
				stream, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, request, opts)
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							errStream = chunk.Err
						}
					}
				}
				if statusCodeFromError(errStream) != http.StatusBadGateway {
					t.Fatalf("turn %d lost the accepted-stream 502: %v", turn, errStream)
				}
				assertNoCooldown(t, manager, "a", "gpt-5")
			}
			if !reflect.DeepEqual(executor.owners, []string{"a", "a"}) {
				t.Fatalf("accepted generation replayed or changed owner: %v", executor.owners)
			}
		})
	}
}
